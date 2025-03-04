package cmd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"os"

	iflag "github.com/skupperproject/skupper/internal/flag"
	"github.com/skupperproject/skupper/internal/kube/client"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	EnsureCookieSecretCommand = "ensure-cookie-secret"
	EnsureCookieSecretDesc    = `When the Secret named <secret name> does not exist, creates a Secret with a
	single key "secret" containing 32 random bytes in base64 URL encoding
	suitable for use with the Oauth2 Proxy as a cookie secret.`
)

func RunEnsureCookieSecret(args []string) error {
	var (
		namespace  string
		kubeconfig string
	)
	flags := flag.NewFlagSet(EnsureCookieSecretCommand, flag.ExitOnError)
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage of %s %s [options...] <secret name>\n", os.Args[0], args[0])
		fmt.Fprintln(flags.Output(), EnsureCookieSecretDesc)
		flags.PrintDefaults()
	}
	iflag.StringVar(flags, &namespace, "namespace", "NAMESPACE", "", "The Kubernetes namespace scope for the controller")
	iflag.StringVar(flags, &kubeconfig, "kubeconfig", "KUBECONFIG", "", "A path to the kubeconfig file to use")
	flags.Parse(args[1:])
	posArgs := flags.Args()
	if len(posArgs) != 1 {
		fmt.Fprintf(flags.Output(), "expected argument for secret name\n")
		flags.Usage()
		os.Exit(1)
	}
	secretName := posArgs[0]
	cli, err := client.NewClient(namespace, "", kubeconfig)
	if err != nil {
		return fmt.Errorf("error creating skupper client: %w", err)
	}
	secretsClient := cli.Kube.CoreV1().Secrets(cli.GetNamespace())

	return ensureCookieSecret(context.Background(), secretsClient, secretName)
}

func ensureCookieSecret(ctx context.Context, client secretCreator, secretName string) error {
	slog.Info("Ensuring cookie secret exists", slog.String("secret", secretName))
	secretBytes := [32]byte{}
	if _, err := rand.Read(secretBytes[:]); err != nil {
		return fmt.Errorf("error generating random cookie secret: %w", err)
	}
	encoded := base64.URLEncoding.EncodeToString(secretBytes[:])
	secret := corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName,
		},
		Type: "Opaque",
		Data: map[string][]byte{
			"secret": []byte(encoded),
		},
	}
	if _, err := client.Create(ctx, &secret, metav1.CreateOptions{}); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("error creating cookie secret: %w", err)
		}
		slog.Info("Cookie secret already exists", slog.String("secret", secretName))
	} else {
		slog.Info("Created secret", slog.String("secret", secretName))
	}
	return nil
}

type secretCreator interface {
	Create(ctx context.Context, secret *corev1.Secret, opts metav1.CreateOptions) (*corev1.Secret, error)
}
