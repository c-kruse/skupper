package certificates

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/skupperproject/skupper/internal/certs"
	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	"github.com/skupperproject/skupper/internal/kube/site/labels"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperfake "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned/fake"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

type metadataContext struct {
	*FakeContext
	policy *labels.LabelsAndAnnotations
}

func (c metadataContext) SnapshotMetadata(namespace string) *labels.LabelsAndAnnotations {
	return c.policy.Snapshot(namespace)
}

func TestMetadataDerivation(t *testing.T) {
	ca := fixtureCASecret(t, "ca", "test")
	old, err := certs.GenerateSecret("leaf", "server", []string{"old.example"}, time.Hour, ca)
	assert.NilError(t, err)
	old.Namespace = "test"
	old.Annotations = map[string]string{"internal.skupper.io/controlled": "true", "internal.skupper.io/hosts": "old.example"}
	for _, existing := range []*corev1.Secret{nil, old} {
		name := "create"
		if existing != nil {
			name = "regenerate"
		}
		t.Run(name, func(t *testing.T) {
			definition := certificate("leaf", "test", "ca", "server", []string{"new.example"}, false, true, nil, nil)
			objects := []runtime.Object{ca}
			if existing != nil {
				objects = append(objects, existing)
			}
			client, err := fakeclient.NewFakeClient("test", objects, []runtime.Object{definition}, "")
			assert.NilError(t, err)
			policy := labels.NewLabelsAndAnnotations("test")
			template := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "policy",
				Labels: map[string]string{"skupper.io/label-template": "true"}, Annotations: map[string]string{"selected": "captured"}},
				Data: map[string]string{"kind": "Secret", "labelSelector": "internal.skupper.io/hosts=new.example"}}
			assert.NilError(t, policy.Update("test/policy", template))
			processor := watchers.NewEventProcessor("metadata", client)
			mgr := NewCertificateManager(processor)
			mgr.SetControllerContext(metadataContext{fakeContext().control("test"), policy})
			mgr.Watch("test")
			stop := make(chan struct{})
			defer close(stop)
			defer processor.Stop()
			processor.StartWatchers(stop)
			assert.Assert(t, processor.WaitForCacheSync(stop))
			snapshot, err := mgr.snapshot("test/leaf")
			assert.NilError(t, err)
			assert.DeepEqual(t, snapshot.secret, existing)
			template.Annotations["selected"] = "later"
			assert.NilError(t, policy.Update("test/policy", template))
			plan := planCertificate(snapshot)
			assert.Equal(t, plan.secret.Annotations["selected"], "captured", "selector must see derived hosts and captured policy")
			assert.Equal(t, plan.secret.Annotations["internal.skupper.io/hosts"], "new.example")
			assert.DeepEqual(t, snapshot.secret, existing)
			assert.Assert(t, reflect.DeepEqual(plan, planCertificate(snapshot)))
		})
	}
}

func TestScopeLifecycle(t *testing.T) {
	ctx := context.Background()
	leaf := certificate("leaf", "test", "ca", "server", []string{"example.test", "10.1.2.3"}, false, true, nil, nil)
	client, err := fakeclient.NewFakeClient("test", nil, []runtime.Object{leaf}, "")
	assert.NilError(t, err)
	secrets := client.GetKubeClient().CoreV1().Secrets("test")
	definitions := client.GetSkupperClient().SkupperV2alpha1().Certificates("test")
	start := func() (*watchers.EventProcessor, *CertificateManagerImpl, func()) {
		p := watchers.NewEventProcessor("cert-test", client)
		m := NewCertificateManager(p)
		m.Watch(metav1.NamespaceAll)
		stop := make(chan struct{})
		p.StartWatchers(stop)
		assert.Assert(t, p.WaitForCacheSync(stop))
		return p, m, func() { close(stop); p.Stop() }
	}
	p, _, stop := start()
	until := func(check func() bool) {
		t.Helper()
		assert.NilError(t, wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, 5*time.Second, true, func(context.Context) (bool, error) {
			p.TestProcessAll()
			return check(), nil
		}))
	}
	ready := func() bool {
		c, err := definitions.Get(ctx, "leaf", metav1.GetOptions{})
		return err == nil && meta.IsStatusConditionTrue(c.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY)
	}
	until(func() bool {
		c, _ := definitions.Get(ctx, "leaf", metav1.GetOptions{})
		condition := meta.FindStatusCondition(c.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY)
		return condition != nil && condition.Status == metav1.ConditionFalse && condition.Message == `CA "test/ca" not found`
	})
	// The CA arrives after startup and after the initial error. No Certificate
	// update or recovery call is needed to wake its dependent scope.
	settle(p)
	_, err = secrets.Create(ctx, fixtureCASecret(t, "ca", "test"), metav1.CreateOptions{})
	assert.NilError(t, err)
	until(ready)
	before, err := secrets.Get(ctx, "leaf", metav1.GetOptions{})
	assert.NilError(t, err)
	decoded, err := certs.DecodeCertificate(before.Data["tls.crt"])
	assert.NilError(t, err)
	assert.Equal(t, decoded.Subject.CommonName, "server")
	assert.NilError(t, decoded.VerifyHostname("example.test"))
	assert.NilError(t, decoded.VerifyHostname("10.1.2.3"))
	assert.DeepEqual(t, before.OwnerReferences, ownerReferences(leaf))
	settle(p)
	statusBefore, err := definitions.Get(ctx, "leaf", metav1.GetOptions{})
	assert.NilError(t, err)
	stop()

	// New manager and informers, same API state: startup must perform no writes.
	kube := client.GetKubeClient().(*kubefake.Clientset)
	writes := 0
	countWrites := func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetVerb() == "create" || a.GetVerb() == "update" || a.GetVerb() == "delete" || a.GetVerb() == "patch" {
			writes++
		}
		return false, nil, nil
	}
	kube.PrependReactor("*", "secrets", countWrites)
	client.GetSkupperClient().(*skupperfake.Clientset).PrependReactor("*", "certificates", countWrites)
	p, mgr, stop := start()
	defer stop()
	settle(p)
	for i := 0; i < 5; i++ {
		p.Enqueue(mgr, "test/leaf")
	}
	p.TestProcessAll()
	assert.Equal(t, writes, 0)
	after, err := secrets.Get(ctx, "leaf", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.DeepEqual(t, after, before)
	statusAfter, err := definitions.Get(ctx, "leaf", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.DeepEqual(t, statusAfter, statusBefore)

	// Fail Secret recreation once. Ready=False must not swallow the error;
	// ordinary rate-limited retry converges without a further external event.
	failed := false
	kube.PrependReactor("create", "secrets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if !failed {
			failed = true
			return true, nil, fmt.Errorf("temporary API failure")
		}
		return false, nil, nil
	})
	assert.NilError(t, secrets.Delete(ctx, "leaf", metav1.DeleteOptions{}))
	until(func() bool {
		s, err := secrets.Get(ctx, "leaf", metav1.GetOptions{})
		return failed && err == nil && !reflect.DeepEqual(s.Data, before.Data) && ready()
	})
	// An expired leaf is renewed, including after a conflicting update. Failed
	// writes cannot mark the informer copy as repaired and suppress the retry.
	ca, err := secrets.Get(ctx, "ca", metav1.GetOptions{})
	assert.NilError(t, err)
	expired, err := certs.GenerateSecret("leaf", "server", leaf.Spec.Hosts, -time.Hour, ca)
	assert.NilError(t, err)
	current, err := secrets.Get(ctx, "leaf", metav1.GetOptions{})
	assert.NilError(t, err)
	current.Data = expired.Data
	_, err = secrets.Update(ctx, current, metav1.UpdateOptions{})
	assert.NilError(t, err)
	conflicted := false
	kube.PrependReactor("update", "secrets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if !conflicted {
			conflicted = true
			return true, nil, apierrors.NewConflict(corev1.Resource("secrets"), "leaf", fmt.Errorf("stale resource version"))
		}
		return false, nil, nil
	})
	until(func() bool {
		s, err := secrets.Get(ctx, "leaf", metav1.GetOptions{})
		if err != nil {
			return false
		}
		cert, err := certs.DecodeCertificate(s.Data["tls.crt"])
		return conflicted && err == nil && cert.NotAfter.After(time.Now()) && ready()
	})
	assert.NilError(t, definitions.Delete(ctx, "leaf", metav1.DeleteOptions{}))
	until(func() bool { _, err := secrets.Get(ctx, "leaf", metav1.GetOptions{}); return apierrors.IsNotFound(err) })
}

func TestExplicitSecretOwnership(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, ownerName, ownerUID           string
		controlled, marked, valid, conflict bool
	}{
		{name: "recreated parent", ownerName: "ca", ownerUID: "old", controlled: true, conflict: true},
		{name: "valid old owner", ownerName: "ca", ownerUID: "old", valid: true, conflict: true},
		{name: "foreign controlled marked owner", ownerName: "other", ownerUID: "new", controlled: true, marked: true, conflict: true},
		{name: "valid foreign marked owner", ownerName: "other", ownerUID: "new", marked: true, valid: true, conflict: true},
		{name: "matching owner", ownerName: "ca", ownerUID: "new"},
		{name: "legacy controlled", controlled: true},
		{name: "valid unmanaged", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := caCertificate("ca", "test", "skupper test CA", nil, nil)
			c.UID = "new"
			s := fixtureCASecret(t, "ca", "test")
			s.OwnerReferences, s.Annotations, s.Labels = nil, map[string]string{}, nil
			if !tc.valid {
				s.Data["tls.crt"] = []byte("invalid")
			}
			if tc.controlled {
				s.Annotations["internal.skupper.io/controlled"] = "true"
			}
			if tc.marked {
				s.Annotations["internal.skupper.io/certificate"] = "true"
			}
			if tc.ownerName != "" {
				s.OwnerReferences = ownerReferences(c)
				s.OwnerReferences[0].Name = tc.ownerName
				s.OwnerReferences[0].UID = types.UID(tc.ownerUID)
			}
			assert.Assert(t, s.DeletionTimestamp == nil)
			client, err := fakeclient.NewFakeClient("test", []runtime.Object{s}, []runtime.Object{c}, "")
			assert.NilError(t, err)
			p := watchers.NewEventProcessor("ownership", client)
			m := NewCertificateManager(p)
			m.Watch(metav1.NamespaceAll)
			stop := make(chan struct{})
			defer close(stop)
			defer p.Stop()
			p.StartWatchers(stop)
			assert.Assert(t, p.WaitForCacheSync(stop))
			settle(p)
			secrets := client.GetKubeClient().CoreV1().Secrets("test")
			definitions := client.GetSkupperClient().SkupperV2alpha1().Certificates("test")
			actual, err := secrets.Get(ctx, "ca", metav1.GetOptions{})
			assert.NilError(t, err)
			observed, err := definitions.Get(ctx, "ca", metav1.GetOptions{})
			assert.NilError(t, err)
			condition := meta.FindStatusCondition(observed.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY)
			assert.Assert(t, condition != nil)
			if tc.conflict {
				assert.Equal(t, condition.Status, metav1.ConditionFalse)
				assert.Equal(t, condition.Reason, "Error")
				assert.Equal(t, condition.Message, "secret has conflicting Certificate ownership")
				assert.DeepEqual(t, actual, s)
				p.Enqueue(m, "test/ca")
				settle(p)
				stable, err := definitions.Get(ctx, "ca", metav1.GetOptions{})
				assert.NilError(t, err)
				assert.DeepEqual(t, stable.Status, observed.Status)
				assert.NilError(t, secrets.Delete(ctx, "ca", metav1.DeleteOptions{}))
				settle(p)
				actual, err = secrets.Get(ctx, "ca", metav1.GetOptions{})
				assert.NilError(t, err)
				assert.DeepEqual(t, actual.OwnerReferences, ownerReferences(c))
				observed, err = definitions.Get(ctx, "ca", metav1.GetOptions{})
				assert.NilError(t, err)
			} else if tc.ownerName == "" && !tc.controlled {
				assert.DeepEqual(t, actual, s)
			}
			assert.Assert(t, meta.IsStatusConditionTrue(observed.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_READY))
			decoded, err := certs.DecodeCertificate(actual.Data["tls.crt"])
			assert.NilError(t, err)
			assert.Equal(t, decoded.Subject.CommonName, c.Spec.Subject)
		})
	}
}

func TestOrphanOwnership(t *testing.T) {
	for _, tc := range []struct {
		name           string
		owners         []string
		marked, remove bool
	}{
		{name: "marker only", marked: true, remove: true},
		{name: "same name old UID", owners: []string{"orphan"}, remove: true},
		{name: "foreign owner overrides marker", owners: []string{"other"}, marked: true},
		{name: "mixed owners", owners: []string{"orphan", "other"}, marked: true},
		{name: "reversed mixed owners", owners: []string{"other", "orphan"}, marked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := secret("orphan", "test", nil, nil, map[string]string{})
			if tc.marked {
				s.Annotations["internal.skupper.io/certificate"] = "true"
			}
			for _, name := range tc.owners {
				s.OwnerReferences = append(s.OwnerReferences, metav1.OwnerReference{APIVersion: "skupper.io/v2alpha1", Kind: "Certificate", Name: name, UID: types.UID(name + "-old")})
			}
			client, err := fakeclient.NewFakeClient("test", []runtime.Object{s}, nil, "")
			assert.NilError(t, err)
			p := watchers.NewEventProcessor("orphan-ownership", client)
			m := NewCertificateManager(p)
			m.Watch(metav1.NamespaceAll)
			stop := make(chan struct{})
			defer close(stop)
			defer p.Stop()
			p.StartWatchers(stop)
			assert.Assert(t, p.WaitForCacheSync(stop))
			settle(p)
			actual, err := client.GetKubeClient().CoreV1().Secrets("test").Get(context.Background(), "orphan", metav1.GetOptions{})
			if tc.remove {
				assert.Assert(t, apierrors.IsNotFound(err))
			} else {
				assert.NilError(t, err)
				assert.DeepEqual(t, actual, s)
			}
		})
	}
}

func TestOrphanRecovery(t *testing.T) {
	orphan := secret("orphan", "test", nil, nil, map[string]string{"internal.skupper.io/certificate": "true"})
	orphan.UID, orphan.ResourceVersion = "orphan-uid", "12"
	unrelated := secret("unrelated", "test", nil, nil, map[string]string{"internal.skupper.io/controlled": "true"})
	client, err := fakeclient.NewFakeClient("test", []runtime.Object{orphan, unrelated}, nil, "")
	assert.NilError(t, err)
	kube := client.GetKubeClient().(*kubefake.Clientset)
	failed := false
	kube.PrependReactor("delete", "secrets", func(a clienttesting.Action) (bool, runtime.Object, error) {
		opts := a.(clienttesting.DeleteAction).GetDeleteOptions()
		assert.Assert(t, opts.Preconditions != nil)
		assert.Equal(t, *opts.Preconditions.UID, orphan.UID)
		assert.Equal(t, *opts.Preconditions.ResourceVersion, orphan.ResourceVersion)
		if !failed {
			failed = true
			return true, nil, fmt.Errorf("temporary delete failure")
		}
		return false, nil, nil
	})
	p := watchers.NewEventProcessor("orphan", client)
	m := NewCertificateManager(p)
	m.Watch(metav1.NamespaceAll)
	stop := make(chan struct{})
	defer close(stop)
	defer p.Stop()
	p.StartWatchers(stop)
	assert.Assert(t, p.WaitForCacheSync(stop))
	// The plan observed no parent, but a parent can appear before execution.
	snapshot, err := m.snapshot("test/orphan")
	assert.NilError(t, err)
	plan := planCertificate(snapshot)
	definitions := client.GetSkupperClient().SkupperV2alpha1().Certificates("test")
	_, err = definitions.Create(context.Background(), caCertificate("orphan", "test", "issuer", nil, nil), metav1.CreateOptions{})
	assert.NilError(t, err)
	assert.NilError(t, m.execute(plan))
	_, err = kube.CoreV1().Secrets("test").Get(context.Background(), "orphan", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.NilError(t, definitions.Delete(context.Background(), "orphan", metav1.DeleteOptions{}))
	getFailed := false
	client.GetSkupperClient().(*skupperfake.Clientset).PrependReactor("get", "certificates", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !getFailed {
			getFailed = true
			return true, nil, fmt.Errorf("cannot confirm absence")
		}
		return false, nil, nil
	})
	assert.Error(t, m.execute(plan), "cannot confirm absence")
	_, err = kube.CoreV1().Secrets("test").Get(context.Background(), "orphan", metav1.GetOptions{})
	assert.NilError(t, err)
	settle(p)
	_, err = kube.CoreV1().Secrets("test").Get(context.Background(), "orphan", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(err))
	actual, err := kube.CoreV1().Secrets("test").Get(context.Background(), "unrelated", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.DeepEqual(t, actual, unrelated)
}

func TestCertificatePlan(t *testing.T) {
	c := caCertificate("ca", "test", "skupper test CA", nil, nil)
	c.SetReady(nil)
	s := fixtureCASecret(t, "ca", "test")
	s.Annotations = map[string]string{"internal.skupper.io/controlled": "true", "internal.skupper.io/certificate": "true"}
	decoded, err := certs.DecodeCertificate(s.Data["tls.crt"])
	assert.NilError(t, err)
	for _, tc := range []struct {
		delta  time.Duration
		action string
	}{{-time.Second, "noop"}, {0, "noop"}, {time.Second, "update-secret"}} {
		snapshot := certificateSnapshot{certificate: c, secret: s, now: decoded.NotAfter.Add(tc.delta)}
		originalCert, originalSecret := c.DeepCopy(), s.DeepCopy()
		first, second := planCertificate(snapshot), planCertificate(snapshot)
		assert.Assert(t, reflect.DeepEqual(first, second), "identical snapshots must produce identical plans")
		assert.Equal(t, first.action, tc.action)
		assert.Equal(t, first.status, false)
		assert.DeepEqual(t, c, originalCert)
		assert.DeepEqual(t, s, originalSecret)
	}
	// Issuer availability is irrelevant for an otherwise-valid existing leaf.
	c.Spec.Signing, c.Spec.Ca = false, "missing"
	plan := planCertificate(certificateSnapshot{certificate: c, secret: s, now: decoded.NotAfter.Add(-time.Second)})
	assert.Equal(t, plan.action, "noop")
	assert.NilError(t, plan.err)
	// An unmanaged incorrect Secret must never be overwritten.
	s.Annotations = nil
	c.Spec.Subject = "different"
	plan = planCertificate(certificateSnapshot{certificate: c, secret: s, now: decoded.NotAfter.Add(-time.Second)})
	assert.Equal(t, plan.action, "noop")
	assert.Error(t, plan.err, "secret exists but is not controlled by skupper")
}
