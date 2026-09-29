package grants

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

type AutoConfigure struct {
	port                 int
	podname              string
	tlsCredentialsSecret string
	ownerRefs            []metav1.OwnerReference
	selector             map[string]string
	watcher              *watchers.SecuredAccessWatcher
}

func (s *AutoConfigure) getConfigurationFromPod(ctx context.Context, clients internalclient.Clients, namespace string) error {
	pod, err := clients.GetKubeClient().CoreV1().Pods(namespace).Get(ctx, s.podname, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, or := range pod.ObjectMeta.OwnerReferences {
		s.ownerRefs = append(s.ownerRefs, metav1.OwnerReference{
			Kind:       or.Kind,
			APIVersion: or.APIVersion,
			Name:       or.Name,
			UID:        or.UID,
		})
	}
	s.selector = make(map[string]string, len(pod.ObjectMeta.Labels))
	for key, value := range pod.ObjectMeta.Labels {
		s.selector[key] = value
	}
	for _, key := range []string{"pod-template-hash", "pod-index", "apps.kubernetes.io/pod-index", "controller-revision-hash", "statefulset.kubernetes.io/pod-name"} {
		delete(s.selector, key)
	}
	return nil
}

func (s *AutoConfigure) ensureCert(ctx context.Context, clients internalclient.Clients, namespace string, desired *skupperv2alpha1.Certificate) error {
	existing, err := clients.GetSkupperClient().SkupperV2alpha1().Certificates(namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = clients.GetSkupperClient().SkupperV2alpha1().Certificates(namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	changed := false
	if !reflect.DeepEqual(existing.ObjectMeta.OwnerReferences, desired.ObjectMeta.OwnerReferences) {
		changed = true
		existing.ObjectMeta.OwnerReferences = desired.ObjectMeta.OwnerReferences
	}
	if !reflect.DeepEqual(existing.Spec, desired.Spec) {
		changed = true
		existing.Spec = desired.Spec
	}
	if !changed {
		return nil
	}
	_, err = clients.GetSkupperClient().SkupperV2alpha1().Certificates(namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (s *AutoConfigure) ensureSecuredAccess(ctx context.Context, clients internalclient.Clients, namespace string, desired *skupperv2alpha1.SecuredAccess) error {
	existing, err := clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace).Create(ctx, desired, metav1.CreateOptions{})
		return err
	} else if err != nil {
		return err
	}
	changed := false
	if !reflect.DeepEqual(existing.ObjectMeta.OwnerReferences, desired.ObjectMeta.OwnerReferences) {
		changed = true
		existing.ObjectMeta.OwnerReferences = desired.ObjectMeta.OwnerReferences
	}
	if !reflect.DeepEqual(existing.Spec, desired.Spec) {
		changed = true
		existing.Spec = desired.Spec
	}
	if !changed {
		return nil
	}
	_, err = clients.GetSkupperClient().SkupperV2alpha1().SecuredAccesses(namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

func (s *AutoConfigure) configure(ctx context.Context, clients internalclient.Clients, namespace string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.getConfigurationFromPod(ctx, clients, namespace); err != nil {
		return err
	}

	cert := &skupperv2alpha1.Certificate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "skupper.io/v2alpha1",
			Kind:       "Certificate",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "skupper-grant-server-ca",
			OwnerReferences: s.ownerRefs,
		},
		Spec: skupperv2alpha1.CertificateSpec{
			Ca:      "",
			Subject: "SkupperGrantServerCA",
			Signing: true,
			Client:  false,
			Server:  false,
		},
	}
	if err := s.ensureCert(ctx, clients, namespace, cert); err != nil {
		return err
	}

	sa := &skupperv2alpha1.SecuredAccess{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "skupper.io/v2alpha1",
			Kind:       "SecuredAccess",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "skupper-grant-server",
			OwnerReferences: s.ownerRefs,
		},
		Spec: skupperv2alpha1.SecuredAccessSpec{
			Selector: s.selector,
			Ports: []skupperv2alpha1.SecuredAccessPort{
				{
					Name: "https",
					Port: s.port,
				},
			},
			Issuer:      "skupper-grant-server-ca",
			Certificate: s.tlsCredentialsSecret,
		},
	}
	if err := s.ensureSecuredAccess(ctx, clients, namespace, sa); err != nil {
		return err
	}
	return nil
}

func newAutoConfigure(handler watchers.SecuredAccessHandler, eventProcessor *watchers.EventProcessor, currentNamespace string, config *GrantConfig) (*AutoConfigure, error) {
	ac := newAutoConfigureDeferred(handler, eventProcessor, currentNamespace, config)
	if err := ac.configure(context.Background(), eventProcessor, currentNamespace); err != nil {
		return nil, fmt.Errorf("Error creating resources for grant server: %s", err)
	}
	return ac, nil
}

func newAutoConfigureDeferred(handler watchers.SecuredAccessHandler, eventProcessor *watchers.EventProcessor, currentNamespace string, config *GrantConfig) *AutoConfigure {
	ac := &AutoConfigure{
		port:                 config.Port,
		tlsCredentialsSecret: config.TlsCredentialsSecret,
		podname:              config.Hostname,
	}
	if ac.tlsCredentialsSecret == "" {
		//TODO: should setting TlsCredentialsSecret be allowed when auto configure is enabled?
		ac.tlsCredentialsSecret = "skupper-grant-server"
	}
	ac.watcher = eventProcessor.WatchSecuredAccessesWithOptions(watchers.SkupperResourceByName("skupper-grant-server"), currentNamespace, handler)
	return ac
}
