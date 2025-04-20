package adaptor

import (
	"context"
	"log"
	"os"
	paths "path"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/skupperproject/skupper/internal/qdr"
)

func InitialiseConfig(client kubernetes.Interface, namespace string, path string, routerConfigMap string) error {
	ctxt := context.Background()
	current, err := client.CoreV1().ConfigMaps(namespace).Get(ctxt, routerConfigMap, metav1.GetOptions{})
	if err != nil {
		return err
	}

	config, err := qdr.GetRouterConfigFromConfigMap(current)
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

	profileManager := newSslProfileManager(path)
	for _, profile := range config.SslProfiles {
		pState, _, _ := profileManager.Register(profile)

		secret, err := client.CoreV1().Secrets(namespace).Get(ctxt, pState.SecretName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if _, err := profileManager.WriteLatest(secret, nil); err != nil {
			return err
		}
	}
	for _, state := range profileManager.profiles {
		log.Printf("SSL Profile %q configured to ordinal %d, present: %t", state.ProfileName, state.Latest, len(state.LatestSum) > 0)
	}
	return nil
}
