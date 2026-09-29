// Package controllerruntime composes the namespace reconciler, authenticated
// router control transport, and process leadership lifecycle.
package controllerruntime

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/controller"
	"github.com/skupperproject/skupper/internal/kube/grants"
	"github.com/skupperproject/skupper/internal/kube/leadership"
	"github.com/skupperproject/skupper/internal/kube/reconcile"
	auth "github.com/skupperproject/skupper/internal/kube/routercontrol"
	protocol "github.com/skupperproject/skupper/internal/routercontrol"
	v2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

type leaderResources struct {
	installation *auth.Installation
	enrollment   net.Listener
	control      net.Listener
}

// Run warms all caches before participating in one election. Only the elected
// process opens the RPC listeners and executes namespace effects. Losing the
// lease returns to main; this process never goes back to standby.
func Run(ctx context.Context, clients internalclient.Clients, config *controller.Config) error {
	if config.Namespace == "" || config.Name == "" || config.PodName == "" || config.PodUID == "" {
		return fmt.Errorf("controller namespace, logical name, Pod name and Pod UID are required")
	}
	if config.Workers < 1 {
		return fmt.Errorf("controller workers must be positive")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	publisher := protocol.NewPublisher()
	var namespaces *controller.NamespaceController
	observations := newObservationCache(func(names ...string) { namespaces.InvalidateNamespaces(names...) })
	bootstrap := reconcile.DefaultRouterControlBootstrap(config.Namespace)
	var err error
	var gatewayOwner *metav1.OwnerReference
	if config.SecuredAccessConfig != nil && slices.Contains(config.SecuredAccessConfig.EnabledAccessTypes, "gateway") {
		gatewayOwner, err = controllerOwner(ctx, clients, config)
		if err != nil {
			return fmt.Errorf("resolve Gateway owner: %w", err)
		}
	}
	namespaces, err = controller.NewNamespaceController(clients, controller.NamespaceControllerOptions{
		WatchNamespace: config.WatchNamespace, ControllerID: config.Namespace + "/" + config.Name,
		RequireExplicitControl: config.WatchNamespace != "" || config.RequireExplicitControl,
		Workers:                config.Workers, DisableSecurityContext: config.DisableSecurityContext, Bootstrap: bootstrap,
		SecuredAccess: config.SecuredAccessConfig, GatewayOwner: gatewayOwner,
	}, publisher, observations)
	if err != nil {
		return err
	}
	var grantFence atomic.Pointer[leadership.Fence]
	lookupSite := func(ctx context.Context, namespace string) (*v2alpha1.Site, error) {
		return lookupGrantSite(ctx, clients, config, namespaces.ActiveSite, namespace)
	}
	grantService, err := grants.NewService(grants.ServiceOptions{
		Clients: clients, CurrentNamespace: config.Namespace, WatchNamespace: config.WatchNamespace,
		Config: config.GrantConfig, LookupSite: lookupSite,
		IsControlled: func(namespace string) bool {
			if !namespaces.IsControlled(namespace) {
				return false
			}
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return checkNamespaceAssignment(checkCtx, clients, config, namespace) == nil
		},
		Generator: grantGenerator(ctx, clients, lookupSite, &grantFence),
	})
	if err != nil {
		return err
	}
	namespaces.StartCaches(ctx)
	if err := grantService.StartCaches(ctx); err != nil {
		return err
	}
	diagnostics := &leadership.Diagnostics{}
	healthListener, err := net.Listen("tcp", ":8080")
	if err != nil {
		return fmt.Errorf("listen for controller health: %w", err)
	}
	health := &http.Server{Handler: diagnostics.Handler(), ReadHeaderTimeout: 5 * time.Second}
	defer health.Close()
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		stop := context.AfterFunc(ctx, func() { _ = health.Close() })
		defer stop()
		err := health.Serve(healthListener)
		if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
			return nil
		}
		return err
	})
	group.Go(func() error {
		defer cancel()
		var prepared leaderResources
		return leadership.Run(ctx, leadership.Config{
			Lock: &resourcelock.LeaseLock{
				LeaseMeta:  metav1.ObjectMeta{Namespace: config.Namespace, Name: "skupper-controller"},
				Client:     clients.GetKubeClient().CoordinationV1(),
				LockConfig: resourcelock.ResourceLockConfig{Identity: config.Name + "/" + config.PodName + "/" + config.PodUID},
			},
			Diagnostics: diagnostics,
			CacheSync: func(ctx context.Context) bool {
				if err := namespaces.WaitForCacheSync(ctx); err != nil {
					slog.Error("Namespace caches did not synchronize", "error", err)
					return false
				}
				return grantService.WaitForCacheSync(ctx)
			},
			Prepare: func(ctx context.Context) error {
				initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				installation, err := auth.EnsureInstallation(initCtx, clients.GetKubeClient(), config.Namespace,
					"skupper-controller-identity", config.Namespace+"/"+config.Name,
					[]string{bootstrap.TLSServerName}, time.Now())
				if err != nil {
					return err
				}
				if err := namespaces.SetRouterControlCA(installation.ServerCAData); err != nil {
					return err
				}
				if err := grantService.Prepare(initCtx); err != nil {
					return err
				}
				// Bind both ports before readiness; cancellation also closes them if
				// leadership is lost before Serve can start.
				enrollment, err := net.Listen("tcp", ":8443")
				if err != nil {
					return err
				}
				control, err := net.Listen("tcp", ":8444")
				if err != nil {
					_ = enrollment.Close()
					return err
				}
				context.AfterFunc(ctx, func() { _ = enrollment.Close(); _ = control.Close() })
				prepared = leaderResources{installation: installation, enrollment: enrollment, control: control}
				return ctx.Err()
			},
			Serve: func(ctx context.Context, fence *leadership.Fence) error {
				grantFence.Store(fence)
				group, ctx := errgroup.WithContext(ctx)
				group.Go(func() error { return grantService.RunLeader(ctx, fence) })
				group.Go(func() error {
					return serveLeader(ctx, clients, config, namespaces, observations, publisher, prepared, fence)
				})
				return group.Wait()
			},
		})
	})
	return group.Wait()
}

// A shared Gateway belongs to the stable StatefulSet, never to a particular
// leader Pod. This lookup is read-only and runs only when Gateway is enabled.
func controllerOwner(ctx context.Context, clients internalclient.Clients, config *controller.Config) (*metav1.OwnerReference, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pod, err := clients.GetKubeClient().CoreV1().Pods(config.Namespace).Get(ctx, config.PodName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if string(pod.UID) != config.PodUID || pod.DeletionTimestamp != nil {
		return nil, fmt.Errorf("controller Pod identity changed or is deleting")
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "StatefulSet" || owner.Name == "" || owner.UID == "" {
		return nil, fmt.Errorf("controller Pod must be owned by an apps/v1 StatefulSet")
	}
	return owner, nil
}

func serveLeader(ctx context.Context, clients internalclient.Clients, config *controller.Config, namespaces *controller.NamespaceController, observations *observationCache, publisher *protocol.Publisher, prepared leaderResources, fence *leadership.Fence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	defer prepared.enrollment.Close()
	defer prepared.control.Close()
	assignment := authorizeAssignment(clients, config)
	authorize := func(ctx context.Context, identity auth.Identity) error {
		if err := assignment(ctx, identity); err != nil {
			return err
		}
		observations.NoteNamespace(string(identity.NamespaceUID), identity.Namespace)
		return nil
	}
	authenticator := &auth.Authenticator{Kube: clients.GetKubeClient(), Installation: prepared.installation, Authorize: authorize, Gate: fence}
	enroller := &auth.Enroller{Kube: clients.GetKubeClient(), Installation: prepared.installation, Authorize: authorize, Gate: fence}
	limits := protocol.DefaultLimits()
	options, err := protocol.GRPCServerOptions(limits)
	if err != nil {
		return err
	}
	options = append(options, grpc.Creds(credentials.NewTLS(authenticator.ServerTLSConfig())), grpc.StreamInterceptor(authenticator.StreamServerInterceptor()))
	grpcServer := grpc.NewServer(options...)
	defer grpcServer.Stop()
	controlServer, err := protocol.NewServer(publisher, auth.AuthorizeSession, observations, limits)
	if err != nil {
		return err
	}
	protocol.RegisterRouterControlServer(grpcServer, controlServer)
	enrollmentTLS := authenticator.ServerTLSConfig()
	enrollmentTLS.ClientAuth = tls.NoClientCert
	enrollmentTLS.ClientCAs = nil
	enrollmentServer := &http.Server{
		Handler:   http.TimeoutHandler(auth.EnrollmentHandler(enroller), 10*time.Second, "enrollment timed out"),
		TLSConfig: enrollmentTLS, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	defer enrollmentServer.Close()
	group, ctx := errgroup.WithContext(ctx)
	stop := context.AfterFunc(ctx, func() { grpcServer.Stop(); _ = enrollmentServer.Close() })
	defer stop()
	group.Go(func() error { return grpcServer.Serve(prepared.control) })
	group.Go(func() error { return enrollmentServer.ServeTLS(prepared.enrollment, "", "") })
	group.Go(func() error { observations.Run(ctx, controlServer); return ctx.Err() })
	group.Go(func() error { return namespaces.RunLeader(ctx) })
	slog.Info("Controller leader is serving", "controller", config.Namespace+"/"+config.Name)
	return group.Wait()
}
