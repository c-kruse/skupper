package adaptor

import (
	"context"
	"log"
	"log/slog"
	"os"
	paths "path"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/cenkalti/backoff/v4"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/secrets"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	"github.com/skupperproject/skupper/internal/qdr"
)

func InitialiseConfig(cli internalclient.Clients, namespace string, path string, routerConfigMap string) error {
	ctxt := context.Background()
	controller := watchers.NewEventProcessor("config-init", cli)
	secretsSync := secrets.NewSync(
		sslSecretsWatcher(namespace, controller),
		slog.New(slog.Default().Handler()).With(slog.String("component", "kube.secrets")),
	)
	stop := make(chan struct{})
	defer close(stop)
	log.Println("Starting secret watcher")
	controller.StartWatchers(stop)
	configMaps := cli.GetKubeClient().CoreV1().ConfigMaps(namespace)
	log.Println("Getting router configuration")
	config, err := getRouterConfig(ctxt, configMaps, routerConfigMap)
	if err != nil {
		return err
	}

	value, err := qdr.MarshalRouterConfig(*config)
	if err != nil {
		return err
	}
	configFile := paths.Join(path, "skrouterd.json")
	if err := os.WriteFile(configFile, []byte(value), 0777); err != nil {
		return err
	}
	log.Printf("Router configuration written to %s", configFile)

	log.Println("Waiting for secret watcher cache")
	controller.WaitForCacheSync(stop)
	secretsSync.Recover()
	err = backoff.Retry(func() error {
		log.Println("Synchroninzing Secrets with router configuration")
		_, err := getRouterConfig(ctxt, configMaps, routerConfigMap)
		if err != nil {
			return err
		}
		delta := secretsSync.Expect(config.SslProfiles)
		return delta.Error()
	}, backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(time.Second*60)))
	if err != nil {
		return err
	}
	log.Printf("Finished synchronizing Secrets with router configuration")
	return nil
}

func getRouterConfig(ctx context.Context, configMaps v1.ConfigMapInterface, name string) (*qdr.RouterConfig, error) {
	current, err := configMaps.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return qdr.GetRouterConfigFromConfigMap(current)
}
