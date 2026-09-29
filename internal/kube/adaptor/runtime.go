package adaptor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	grpcCredentials "google.golang.org/grpc/credentials"
	"k8s.io/apimachinery/pkg/util/uuid"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	kuberoutercontrol "github.com/skupperproject/skupper/internal/kube/routercontrol"
	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
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
	session, connection, _, _, err := runtime.connect(ctx)
	if err != nil {
		return err
	}
	defer connection.Close()
	for {
		event, err := session.NextEvent()
		if err != nil {
			return err
		}
		if event.Refresh != nil || event.Intent == nil || event.Intent.Unavailable {
			continue
		}
		if err := validateSupportedIntent(event.Intent.Intent); err != nil {
			_ = session.Reject(*event.Intent, err.Error())
			continue
		}
		if err := session.Accept(*event.Intent); err != nil {
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
		session, connection, expiry, sessionIncarnation, err := runtime.connect(ctx)
		if err != nil {
			if !sleepContext(ctx, backoff) {
				return ctx.Err()
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		err = runtime.runSession(ctx, session, expiry, sessionIncarnation)
		_ = connection.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !sleepContext(ctx, backoff) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func (r *controlRuntime) runSession(ctx context.Context, session *routercontrol.ClientSession, expiry time.Time, sessionIncarnation string) error {
	type received struct {
		event routercontrol.ServerEvent
		err   error
	}
	events := make(chan received)
	go func() {
		for {
			event, err := session.NextEvent()
			select {
			case events <- received{event, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	renew := time.NewTimer(max(time.Until(expiry.Add(-15*time.Minute)), time.Second))
	defer renew.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	reconcile := time.NewTicker(10 * time.Second)
	defer reconcile.Stop()
	engine := Engine{Router: r.router, Credentials: r.credentials, RouterIncarnation: sessionIncarnation, StartupConfig: r.startup}
	var accepted *routercontrol.IntentUpdate
	var compiled CompiledIntent
	var sample uint64
	for {
		select {
		case <-ctx.Done():
			_ = session.CloseSend()
			return ctx.Err()
		case <-renew.C:
			_ = session.CloseSend()
			return errors.New("router-control credential renewal due")
		case <-heartbeat.C:
			if err := session.SendHeartbeat(); err != nil {
				return err
			}
		case <-reconcile.C:
			if accepted != nil {
				report, next := engine.RealizeDetailed(ctx, session.SessionID(), accepted.Sequence, accepted.Intent, accepted.Digest)
				compiled = next
				if report.RouterIncarnation != sessionIncarnation {
					return errors.New("local router management reconnected; start a fresh control session")
				}
				if err := session.SendApplication(report); err != nil {
					return err
				}
			}
		case item := <-events:
			if item.err != nil {
				return item.err
			}
			if item.event.Refresh != nil {
				sample++
				var observation routercontrol.ObservationSnapshot
				switch item.event.Refresh.Scope {
				case routercontrol.ObservationScopeResources:
					observation = r.router.ObserveResources(session.SessionID(), sessionIncarnation, sample, compiled, item.event.Refresh.RequestID)
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
				if err := session.SendObservation(observation); err != nil {
					return err
				}
			} else if item.event.Intent != nil && !item.event.Intent.Unavailable {
				if err := validateSupportedIntent(item.event.Intent.Intent); err != nil {
					if rejectErr := session.Reject(*item.event.Intent, err.Error()); rejectErr != nil {
						return rejectErr
					}
					continue
				}
				if err := session.Accept(*item.event.Intent); err != nil {
					return err
				}
				accepted = item.event.Intent
				report, next := engine.RealizeDetailed(ctx, session.SessionID(), accepted.Sequence, accepted.Intent, accepted.Digest)
				compiled = next
				if report.RouterIncarnation != sessionIncarnation {
					return errors.New("local router management reconnected; start a fresh control session")
				}
				if err := session.SendApplication(report); err != nil {
					return err
				}
			}
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
