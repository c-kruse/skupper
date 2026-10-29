package certificates

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/skupperproject/skupper/internal/certs"
	"github.com/skupperproject/skupper/internal/kube/site/labels"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type certificateSnapshot struct {
	certificate    *skupperv2alpha1.Certificate
	secret, ca     *corev1.Secret
	metadataPolicy *labels.LabelsAndAnnotations
	now            time.Time
}

type certificatePlan struct {
	certificate        *skupperv2alpha1.Certificate
	secret, ca         *corev1.Secret
	action             string
	regenerate, status bool
	err                error
}

func (m *CertificateManagerImpl) snapshot(key string) (certificateSnapshot, error) {
	s := certificateSnapshot{now: time.Now()}
	certificate, err := m.certificateWatcher.Get(key)
	if err != nil {
		return s, err
	}
	secret, err := m.secretWatcher.Get(key)
	if err != nil {
		return s, err
	}
	s.certificate, s.secret = certificate.DeepCopy(), secret.DeepCopy()
	if certificate != nil {
		if !certificate.Spec.Signing {
			ca, err := m.secretWatcher.Get(certificate.Namespace + "/" + certificate.Spec.Ca)
			if err != nil {
				return s, err
			}
			s.ca = ca.DeepCopy()
		}
		if m.context != nil {
			s.metadataPolicy = m.context.SnapshotMetadata(certificate.Namespace)
		}
	}
	return s, nil
}

// planCertificate derives desired metadata and validity from immutable inputs.
// Crypto and condition transition timestamps are execution results, not plan inputs.
func planCertificate(s certificateSnapshot) certificatePlan {
	p := certificatePlan{certificate: s.certificate.DeepCopy(), secret: s.secret.DeepCopy(), ca: s.ca.DeepCopy(), action: "noop"}
	c := p.certificate
	if c == nil {
		if p.secret != nil && p.secret.DeletionTimestamp == nil && certificateSecret(p.secret) {
			p.action = "delete-secret"
		}
		return p
	}
	if c.DeletionTimestamp != nil || (p.secret != nil && p.secret.DeletionTimestamp != nil) {
		return p
	}
	owners := certificateToOwnerMapping(c)
	if owners.IsControlled {
		present := map[string]bool{}
		for _, ref := range c.OwnerReferences {
			present[string(ref.UID)] = true
		}
		for uid := range owners.PerOwnerHosts {
			if !present[uid] {
				delete(owners.PerOwnerHosts, uid)
			}
		}
		if owners.ApplyMetadata(c) {
			c.Spec.Hosts = owners.CombinedHosts()
			p.action = "update-certificate"
			return p
		}
	}
	if p.secret != nil {
		for _, ref := range p.secret.OwnerReferences {
			if ref.APIVersion == "skupper.io/v2alpha1" && ref.Kind == "Certificate" && (ref.Name != c.Name || ref.UID != c.UID) {
				p.err = errors.New("secret has conflicting Certificate ownership")
				break
			}
		}
	}
	if p.secret == nil {
		p.action, p.regenerate = "create-secret", true
		p.secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: c.Name, Namespace: c.Namespace,
			OwnerReferences: ownerReferences(c), Labels: map[string]string{}, Annotations: map[string]string{
				"internal.skupper.io/controlled": "true", "internal.skupper.io/certificate": "true",
				"internal.skupper.io/hosts": strings.Join(c.Spec.Hosts, ","),
			}}}
	} else if p.err == nil && !isSecretCorrect(c, p.secret, s.now) {
		if !isSecretControlled(p.secret) {
			p.err = errors.New("secret exists but is not controlled by skupper")
		} else {
			p.action, p.regenerate = "update-secret", true
			setAnnotation(&p.secret.ObjectMeta, "internal.skupper.io/hosts", strings.Join(c.Spec.Hosts, ","))
		}
	}
	if p.regenerate && !c.Spec.Signing && p.ca == nil {
		p.err = fmt.Errorf("CA %q not found", c.Namespace+"/"+c.Spec.Ca)
	}
	if p.err == nil && (s.secret == nil || isSecretControlled(p.secret)) {
		// Evaluate the captured policy against candidate metadata, after deriving
		// built-in annotations but before adding the legacy ownership marker.
		metadata := p.secret.ObjectMeta.DeepCopy()
		if s.metadataPolicy != nil {
			if metadata.Labels == nil {
				metadata.Labels = map[string]string{}
			}
			if metadata.Annotations == nil {
				metadata.Annotations = map[string]string{}
			}
			s.metadataPolicy.SetLabels(c.Namespace, c.Name, "Secret", metadata.Labels)
			s.metadataPolicy.SetAnnotations(c.Namespace, c.Name, "Secret", metadata.Annotations)
		}
		// Persist the association for legacy controlled Secrets while their
		// Certificate is present, so deletion never depends on process history.
		setAnnotation(&p.secret.ObjectMeta, "internal.skupper.io/certificate", "true")
		for k, v := range metadata.Labels {
			if p.secret.Labels == nil {
				p.secret.Labels = map[string]string{}
			}
			p.secret.Labels[k] = v
		}
		for k, v := range metadata.Annotations {
			setAnnotation(&p.secret.ObjectMeta, k, v)
		}
		if p.action == "noop" && !reflect.DeepEqual(p.secret.ObjectMeta, s.secret.ObjectMeta) {
			p.action = "update-secret"
		}
	}
	if p.err != nil {
		p.action, p.regenerate = "noop", false
	}
	desired := skupperv2alpha1.ErrorOrReadyCondition(p.err)
	current := meta.FindStatusCondition(c.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY)
	p.status = current == nil || current.Status != desired.Status || current.Reason != string(desired.Reason) || current.Message != desired.Message || current.ObservedGeneration != c.Generation
	return p
}

// Only durable Certificate ownership permits cleanup after absence/restart.
func certificateSecret(secret *corev1.Secret) bool {
	identified := secret.Annotations["internal.skupper.io/certificate"] == "true"
	for _, ref := range secret.OwnerReferences {
		if ref.APIVersion == "skupper.io/v2alpha1" && ref.Kind == "Certificate" {
			if ref.Name != secret.Name {
				return false
			}
			identified = true
		}
	}
	return identified
}

func (m *CertificateManagerImpl) execute(p certificatePlan) error {
	ctx := context.Background()
	if p.action == "delete-secret" {
		s := p.secret
		// Absence in the informer can lag creation. This execution-time safety
		// check must succeed with NotFound before we remove a surviving Secret.
		_, err := m.processor.GetSkupperClient().SkupperV2alpha1().Certificates(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
		if err == nil {
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		err = m.processor.GetKubeClient().CoreV1().Secrets(s.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &s.UID, ResourceVersion: &s.ResourceVersion}})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	c := p.certificate
	if c == nil || c.DeletionTimestamp != nil {
		return nil
	}
	client := m.processor.GetSkupperClient().SkupperV2alpha1().Certificates(c.Namespace)
	if p.action == "update-certificate" {
		_, err := client.Update(ctx, c, metav1.UpdateOptions{})
		return err
	}
	err := p.err
	if p.regenerate {
		var generated *corev1.Secret
		if c.Spec.Signing {
			generated, err = certs.GenerateSecret(c.Name, c.Spec.Subject, nil, 0, nil)
		} else {
			generated, err = certs.GenerateSecret(c.Name, c.Spec.Subject, c.Spec.Hosts, time.Hour*24*365*5, p.ca)
		}
		if err == nil {
			p.secret.Data = generated.Data
			if p.action == "create-secret" {
				p.secret.Type = generated.Type
			}
		}
	}
	if err == nil {
		switch p.action {
		case "create-secret":
			_, err = m.processor.GetKubeClient().CoreV1().Secrets(c.Namespace).Create(ctx, p.secret, metav1.CreateOptions{})
		case "update-secret":
			_, err = m.processor.GetKubeClient().CoreV1().Secrets(c.Namespace).Update(ctx, p.secret, metav1.UpdateOptions{})
		}
	}
	if (p.status || err != nil) && c.SetReady(err) {
		_, statusErr := client.UpdateStatus(ctx, c, metav1.UpdateOptions{})
		err = errors.Join(err, statusErr)
	}
	return err
}

func isSecretCorrect(certificate *skupperv2alpha1.Certificate, secret *corev1.Secret, now time.Time) bool {
	cert, err := certs.DecodeCertificate(secret.Data["tls.crt"])
	if err != nil || now.After(cert.NotAfter) || certificate.Spec.Subject != cert.Subject.CommonName {
		return false
	}
	validFor := map[string]bool{}
	for _, host := range cert.DNSNames {
		if host != "" {
			validFor[host] = true
		}
	}
	for _, ip := range cert.IPAddresses {
		validFor[ip.String()] = true
	}
	if len(certificate.Spec.Hosts) != len(validFor) {
		return false
	}
	for _, host := range certificate.Spec.Hosts {
		if !validFor[host] {
			return false
		}
	}
	return true
}
