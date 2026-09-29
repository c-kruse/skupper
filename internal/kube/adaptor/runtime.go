package adaptor

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	grpcCredentials "google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/util/uuid"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	kuberoutercontrol "github.com/skupperproject/skupper/internal/kube/routercontrol"
	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

const (
	controlHandshakeTimeout         = 20 * time.Second
	controlSendTimeout              = 15 * time.Second
	controlReceiveInactivityTimeout = 30 * time.Second
	initialIntentTimeout            = 30 * time.Second
)

type ControlConfig struct {
	EnrollmentURL  string
	ControlAddress string
	ServerName     string
	TokenPath      string
	PublicCAPath   string
	Target         routercontrol.TargetIdentity
	ConfigDir      string
}

type controlRuntime struct {
	config      ControlConfig
	enrollment  *kuberoutercontrol.EnrollmentClient
	credentials *SecretCredentialProvider
	router      *AMQPLocalRouter
	startup     *qdr.RouterConfig
	incarnation string
	instance    string
}

type connectedControlSession struct {
	session     *routercontrol.ClientSession
	connection  *grpc.ClientConn
	expiry      time.Time
	incarnation string
	ctx         context.Context
	cancel      context.CancelFunc
}

type receivedControlEvent struct {
	event routercontrol.ServerEvent
	err   error
}

type controlEventReceiver interface {
	NextEvent() (routercontrol.ServerEvent, error)
}

func newControlRuntime(config ControlConfig, secrets corev1client.SecretInterface) (*controlRuntime, error) {
	if err := routercontrol.ValidateIntent(routercontrol.RouterIntent{SchemaVersion: routercontrol.SchemaVersion, Target: config.Target, Settings: routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior}}); err != nil {
		return nil, fmt.Errorf("invalid router target: %w", err)
	}
	enrollment, err := kuberoutercontrol.NewEnrollmentClient(config.EnrollmentURL, config.TokenPath, config.PublicCAPath, config.ServerName)
	if err != nil {
		return nil, err
	}
	return &controlRuntime{config: config, enrollment: enrollment, credentials: NewSecretCredentialProvider(secrets, config.ConfigDir), router: &AMQPLocalRouter{Pool: qdr.NewAgentPool("amqp://localhost:5672", nil)}, incarnation: "unverified-" + string(uuid.NewUUID()), instance: string(uuid.NewUUID())}, nil
}

func (r *controlRuntime) connect(ctx context.Context) (*routercontrol.ClientSession, *grpc.ClientConn, time.Time, string, error) {
	credential, err := r.enrollment.Enroll(ctx)
	if err != nil {
		return nil, nil, time.Time{}, "", err
	}
	caData, err := os.ReadFile(r.config.PublicCAPath)
	if err != nil {
		return nil, nil, time.Time{}, "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caData) {
		return nil, nil, time.Time{}, "", errors.New("router-control CA bundle is empty")
	}
	tlsCertificate := credential.TLSCertificate()
	connection, err := grpc.NewClient(r.config.ControlAddress, grpc.WithTransportCredentials(grpcCredentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: r.config.ServerName, Certificates: []tls.Certificate{tlsCertificate}})))
	if err != nil {
		return nil, nil, time.Time{}, "", err
	}
	incarnation := r.incarnation
	if current, _ := r.router.CurrentRouterIncarnation(); current != "" {
		incarnation = current
	}
	hello := routercontrol.Hello{AdaptorInstanceID: r.instance, RouterIncarnation: incarnation, Target: r.config.Target}
	session, err := routercontrol.OpenClientSession(ctx, connection, hello, routercontrol.DefaultLimits())
	if err != nil {
		connection.Close()
		return nil, nil, time.Time{}, "", err
	}
	return session, connection, credential.Leaf.NotAfter, incarnation, nil
}

func (r *controlRuntime) connectBounded(parent context.Context) (*connectedControlSession, error) {
	ctx, cancel := context.WithCancel(parent)
	result := make(chan struct {
		session     *routercontrol.ClientSession
		connection  *grpc.ClientConn
		expiry      time.Time
		incarnation string
		err         error
	}, 1)
	go func() {
		session, connection, expiry, incarnation, err := r.connect(ctx)
		result <- struct {
			session     *routercontrol.ClientSession
			connection  *grpc.ClientConn
			expiry      time.Time
			incarnation string
			err         error
		}{session, connection, expiry, incarnation, err}
	}()
	timer := time.NewTimer(controlHandshakeTimeout)
	defer timer.Stop()
	select {
	case outcome := <-result:
		if outcome.err != nil {
			cancel()
			return nil, outcome.err
		}
		return &connectedControlSession{session: outcome.session, connection: outcome.connection, expiry: outcome.expiry, incarnation: outcome.incarnation, ctx: ctx, cancel: cancel}, nil
	case <-timer.C:
		cancel()
		outcome := <-result
		if outcome.connection != nil {
			_ = outcome.connection.Close()
		}
		return nil, fmt.Errorf("router-control handshake exceeded %s", controlHandshakeTimeout)
	case <-parent.Done():
		cancel()
		outcome := <-result
		if outcome.connection != nil {
			_ = outcome.connection.Close()
		}
		return nil, parent.Err()
	}
}

func startControlReceiver(ctx context.Context, session controlEventReceiver) (<-chan receivedControlEvent, <-chan struct{}) {
	events := make(chan receivedControlEvent, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		event, err := session.NextEvent()
		select {
		case events <- receivedControlEvent{event: event, err: err}:
		case <-ctx.Done():
		}
	}()
	return events, done
}

func boundedSessionCall(ctx context.Context, cancel context.CancelFunc, operation func() error) error {
	return boundedCall(ctx, cancel, controlSendTimeout, operation)
}

func boundedCall(ctx context.Context, cancel context.CancelFunc, timeout time.Duration, operation func() error) error {
	result := make(chan error, 1)
	go func() { result <- operation() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		cancel()
		<-result
		return fmt.Errorf("router-control operation exceeded %s", timeout)
	case <-ctx.Done():
		cancel()
		<-result
		return ctx.Err()
	}
}

func certificateRenewalDelay(now, expiry time.Time, jitter float64) time.Duration {
	remaining := expiry.Sub(now)
	if remaining <= 0 {
		return 0
	}
	if jitter < -1 {
		jitter = -1
	} else if jitter > 1 {
		jitter = 1
	}
	margin := remaining / 3
	margin += time.Duration(float64(margin) * 0.1 * jitter)
	if margin > 15*time.Minute {
		margin = 15 * time.Minute
	}
	delay := remaining - margin
	if delay <= 0 {
		return remaining / 2
	}
	return delay
}

func renewalJitter() float64 {
	var value [1]byte
	if _, err := cryptorand.Read(value[:]); err != nil {
		return 0
	}
	return float64(value[0])/127.5 - 1
}

func reconnectBackoffDelay(base time.Duration, jitter float64) time.Duration {
	if jitter < -1 {
		jitter = -1
	} else if jitter > 1 {
		jitter = 1
	}
	return base + time.Duration(float64(base)*0.2*jitter)
}

func resetInactivityTimer(timer *time.Timer, timeout time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(timeout)
}

func boundedErrorReason(err error) string {
	if err == nil {
		return ""
	}
	reason := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	const limit = 512
	if len(reason) > limit {
		reason = reason[:limit] + "..."
	}
	return reason
}

func validateSupportedIntent(intent routercontrol.RouterIntent) error {
	for _, listener := range intent.ServiceListeners {
		if listener.Protocol != routercontrol.ProtocolTCP {
			return fmt.Errorf("service listener %q protocol %q is unsupported", listener.ID, listener.Protocol)
		}
	}
	for _, connector := range intent.ServiceConnectors {
		if connector.Protocol != routercontrol.ProtocolTCP {
			return fmt.Errorf("service connector %q protocol %q is unsupported", connector.ID, connector.Protocol)
		}
	}
	return nil
}

func RunConfigInit(ctx context.Context, config ControlConfig, secrets corev1client.SecretInterface) error {
	runtime, err := newControlRuntime(config, secrets)
	if err != nil {
		return err
	}
	control, err := runtime.connectBounded(ctx)
	if err != nil {
		return err
	}
	events, receiverDone := startControlReceiver(control.ctx, control.session)
	defer func() {
		control.cancel()
		if receiverDone != nil {
			<-receiverDone
		}
		_ = control.connection.Close()
	}()
	timer := time.NewTimer(initialIntentTimeout)
	defer timer.Stop()
	for {
		var item receivedControlEvent
		select {
		case <-control.ctx.Done():
			return control.ctx.Err()
		case <-timer.C:
			return fmt.Errorf("initial router intent not received within %s", initialIntentTimeout)
		case item = <-events:
		}
		<-receiverDone
		receiverDone = nil
		if item.err != nil {
			return item.err
		}
		event := item.event
		if event.Refresh != nil || event.Intent == nil || event.Intent.Unavailable {
			events, receiverDone = startControlReceiver(control.ctx, control.session)
			continue
		}
		if err := validateSupportedIntent(event.Intent.Intent); err != nil {
			if rejectErr := boundedSessionCall(control.ctx, control.cancel, func() error { return control.session.Reject(*event.Intent, err.Error()) }); rejectErr != nil {
				return rejectErr
			}
			events, receiverDone = startControlReceiver(control.ctx, control.session)
			continue
		}
		if err := boundedSessionCall(control.ctx, control.cancel, func() error { return control.session.Accept(*event.Intent) }); err != nil {
			return err
		}
		source := staticInitialIntent{intent: event.Intent.Intent, digest: event.Intent.Digest}
		return InitialiseFromIntent(ctx, source, runtime.credentials, config.ConfigDir)
	}
}

type staticInitialIntent struct {
	intent routercontrol.RouterIntent
	digest routercontrol.Digest
}

func (s staticInitialIntent) FetchInitialIntent(context.Context) (routercontrol.RouterIntent, routercontrol.Digest, error) {
	return s.intent, s.digest, nil
}

func RunSidecar(ctx context.Context, config ControlConfig, secrets corev1client.SecretInterface) error {
	runtime, err := newControlRuntime(config, secrets)
	if err != nil {
		return err
	}
	startupData, err := os.ReadFile(filepath.Join(config.ConfigDir, "skrouterd.json"))
	if err != nil {
		return fmt.Errorf("read router startup config: %w", err)
	}
	startup, err := qdr.UnmarshalRouterConfig(string(startupData))
	if err != nil {
		return fmt.Errorf("parse router startup config: %w", err)
	}
	runtime.startup = &startup
	backoff := time.Second
	for ctx.Err() == nil {
		_, _ = runtime.router.Read()
		control, err := runtime.connectBounded(ctx)
		if err != nil {
			delay := reconnectBackoffDelay(backoff, renewalJitter())
			slog.Warn("router-control reconnect", slog.String("phase", "connect"), slog.String("reason", boundedErrorReason(err)), slog.Duration("retryAfter", delay))
			if !sleepContext(ctx, delay) {
				return ctx.Err()
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		err = runtime.runSession(control.ctx, control.cancel, control.session, control.expiry, control.incarnation)
		_ = control.connection.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := reconnectBackoffDelay(backoff, renewalJitter())
		slog.Warn("router-control reconnect", slog.String("phase", "session"), slog.String("reason", boundedErrorReason(err)), slog.Duration("retryAfter", delay))
		if !sleepContext(ctx, delay) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (r *controlRuntime) runSession(ctx context.Context, cancel context.CancelFunc, session *routercontrol.ClientSession, expiry time.Time, sessionIncarnation string) error {
	events, receiverDone := startControlReceiver(ctx, session)
	defer func() {
		cancel()
		if receiverDone != nil {
			<-receiverDone
		}
	}()
	renew := time.NewTimer(certificateRenewalDelay(time.Now(), expiry, renewalJitter()))
	defer renew.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	reconcile := time.NewTicker(10 * time.Second)
	defer reconcile.Stop()
	receiveInactivity := time.NewTimer(controlReceiveInactivityTimeout)
	defer receiveInactivity.Stop()
	engine := Engine{Router: r.router, Credentials: r.credentials, RouterIncarnation: sessionIncarnation, StartupConfig: r.startup}
	var accepted *routercontrol.IntentUpdate
	var compiled CompiledIntent
	var compiledValid bool
	var sample uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-renew.C:
			return errors.New("router-control credential renewal due")
		case <-receiveInactivity.C:
			return fmt.Errorf("router-control receive inactive for %s", controlReceiveInactivityTimeout)
		case <-heartbeat.C:
			if err := boundedSessionCall(ctx, cancel, session.SendHeartbeat); err != nil {
				return err
			}
		case <-reconcile.C:
			if accepted != nil {
				report, next := engine.RealizeDetailed(ctx, session.SessionID(), accepted.Sequence, accepted.Intent, accepted.Digest)
				if next.RealizationID != "" {
					compiled = next
					compiledValid = true
				}
				if report.RouterIncarnation != sessionIncarnation {
					return errors.New("local router management reconnected; start a fresh control session")
				}
				if err := boundedSessionCall(ctx, cancel, func() error { return session.SendApplication(report) }); err != nil {
					return err
				}
			}
		case item := <-events:
			<-receiverDone
			receiverDone = nil
			resetInactivityTimer(receiveInactivity, controlReceiveInactivityTimeout)
			if item.err != nil {
				return item.err
			}
			if item.event.Refresh != nil {
				sample++
				var observation routercontrol.ObservationSnapshot
				switch item.event.Refresh.Scope {
				case routercontrol.ObservationScopeResources:
					if !compiledValid {
						observation = routercontrol.ObservationSnapshot{Scope: routercontrol.ObservationScopeResources, SampleSequence: sample, RefreshRequestID: item.event.Refresh.RequestID, Knowledge: routercontrol.KnowledgeUnknown, RouterIncarnation: sessionIncarnation, Reason: "accepted intent has no compiled realization"}
					} else {
						observation = r.router.ObserveResources(session.SessionID(), sessionIncarnation, sample, compiled, item.event.Refresh.RequestID)
					}
				case routercontrol.ObservationScopeAddresses:
					if accepted == nil {
						observation = routercontrol.ObservationSnapshot{Scope: routercontrol.ObservationScopeAddresses, SampleSequence: sample, RefreshRequestID: item.event.Refresh.RequestID, Knowledge: routercontrol.KnowledgeUnknown, RouterIncarnation: sessionIncarnation, Reason: "intent unavailable"}
					} else {
						observation = r.router.ObserveAddresses(session.SessionID(), sessionIncarnation, sample, accepted.Intent, item.event.Refresh.RequestID)
					}
				default:
					observation = routercontrol.ObservationSnapshot{Scope: item.event.Refresh.Scope, SampleSequence: sample, RefreshRequestID: item.event.Refresh.RequestID, Knowledge: routercontrol.KnowledgeUnknown, RouterIncarnation: sessionIncarnation, Reason: "unsupported observation scope"}
				}
				if observation.RouterIncarnation != sessionIncarnation {
					return errors.New("local router management reconnected; start a fresh control session")
				}
				if err := boundedSessionCall(ctx, cancel, func() error { return session.SendObservation(observation) }); err != nil {
					return err
				}
			} else if item.event.Intent != nil && !item.event.Intent.Unavailable {
				if err := validateSupportedIntent(item.event.Intent.Intent); err != nil {
					if rejectErr := boundedSessionCall(ctx, cancel, func() error { return session.Reject(*item.event.Intent, err.Error()) }); rejectErr != nil {
						return rejectErr
					}
					events, receiverDone = startControlReceiver(ctx, session)
					continue
				}
				if err := boundedSessionCall(ctx, cancel, func() error { return session.Accept(*item.event.Intent) }); err != nil {
					return err
				}
				accepted = item.event.Intent
				r.router.InvalidateRouterVerification()
				compiledValid = false
				report, next := engine.RealizeDetailed(ctx, session.SessionID(), accepted.Sequence, accepted.Intent, accepted.Digest)
				if next.RealizationID != "" {
					compiled = next
					compiledValid = true
				}
				if report.RouterIncarnation != sessionIncarnation {
					return errors.New("local router management reconnected; start a fresh control session")
				}
				if err := boundedSessionCall(ctx, cancel, func() error { return session.SendApplication(report) }); err != nil {
					return err
				}
			}
			events, receiverDone = startControlReceiver(ctx, session)
		}
	}
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
