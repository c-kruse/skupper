package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	iflag "github.com/skupperproject/skupper/internal/flag"
	"github.com/skupperproject/skupper/internal/kube/adaptor"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/routercontrol"
	"github.com/skupperproject/skupper/internal/version"
)

func main() {
	flags := flag.NewFlagSet("", flag.ExitOnError)
	var namespace, namespaceUID, siteUID, routerGroup, kubeconfig, configDir string
	var enrollmentURL, controlAddress, serverName, tokenPath, caPath string
	iflag.StringVar(flags, &namespace, "namespace", "NAMESPACE", "", "The router namespace")
	iflag.StringVar(flags, &namespaceUID, "namespace-uid", "SKUPPER_NAMESPACE_UID", "", "The router namespace UID")
	iflag.StringVar(flags, &siteUID, "site-uid", "SKUPPER_SITE_UID", "", "The active Site UID")
	iflag.StringVar(flags, &routerGroup, "router-group", "SKUPPER_ROUTER_GROUP", "", "The logical router group")
	iflag.StringVar(flags, &kubeconfig, "kubeconfig", "KUBECONFIG", "", "A path to kubeconfig")
	iflag.StringVar(flags, &configDir, "config-dir", "SKUPPER_CONFIG_DIR", "/etc/skupper-router-certs", "Router configuration and traffic credential directory")
	iflag.StringVar(flags, &enrollmentURL, "enrollment-url", "SKUPPER_CONTROLLER_ENROLLMENT_URL", "", "Controller enrollment URL (defaults to namespace-qualified service DNS)")
	iflag.StringVar(flags, &controlAddress, "control-address", "SKUPPER_CONTROLLER_CONTROL_ADDRESS", "", "Controller mTLS gRPC address (defaults to namespace-qualified service DNS)")
	iflag.StringVar(flags, &serverName, "control-server-name", "SKUPPER_CONTROLLER_SERVER_NAME", "", "Expected controller TLS DNS name (defaults to namespace-qualified service DNS)")
	iflag.StringVar(flags, &tokenPath, "enrollment-token", "SKUPPER_CONTROLLER_ENROLLMENT_TOKEN", "/var/run/secrets/skupper-controller/enrollment-token", "Projected bound-token path")
	iflag.StringVar(flags, &caPath, "control-ca", "SKUPPER_CONTROLLER_CA", "/etc/skupper-controller/ca.crt", "Controller public server CA bundle")
	isVersion := flags.Bool("version", false, "Report the version")
	isInit := flags.Bool("init", false, "Fetch initial intent and write router startup configuration")
	flags.Parse(os.Args[1:])
	if *isVersion {
		fmt.Println(version.Version)
		return
	}

	client, err := internalclient.NewClient(namespace, "", kubeconfig)
	if err != nil {
		slog.Error("create Kubernetes client", slog.Any("error", err))
		os.Exit(1)
	}
	controllerHost := "skupper-controller." + client.GetNamespace() + ".svc"
	if enrollmentURL == "" {
		enrollmentURL = "https://" + controllerHost + ":8443"
	}
	if controlAddress == "" {
		controlAddress = controllerHost + ":8444"
	}
	if serverName == "" {
		serverName = controllerHost
	}
	config := adaptor.ControlConfig{
		EnrollmentURL: enrollmentURL, ControlAddress: controlAddress, ServerName: serverName,
		TokenPath: tokenPath, PublicCAPath: caPath, ConfigDir: configDir,
		Target: routercontrol.TargetIdentity{NamespaceUID: namespaceUID, SiteUID: siteUID, RouterGroup: routerGroup},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	secrets := client.GetKubeClient().CoreV1().Secrets(client.GetNamespace())
	if *isInit {
		err = adaptor.RunConfigInit(ctx, config, secrets)
	} else {
		go serveHealth(ctx)
		err = adaptor.RunSidecar(ctx, config, secrets)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("router adaptor stopped", slog.Any("error", err))
		os.Exit(1)
	}
}

func serveHealth(ctx context.Context) {
	server := &http.Server{Addr: ":9191", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { <-ctx.Done(); _ = server.Close() }()
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("health server", slog.Any("error", err))
	}
}
