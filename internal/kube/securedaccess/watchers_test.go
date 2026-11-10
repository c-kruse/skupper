package securedaccess

import (
	"context"
	"errors"
	"testing"

	"github.com/skupperproject/skupper/internal/kube/resource"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestScopeRoutingInventoriesSurvivingOutputs(t *testing.T) {
	h := newHarness(t, nil)
	sa := testAccess("local")
	sa.Spec.Issuer = "issuer"
	h.put(sa)
	h.settle("edge")
	// A legacy label spelling must not exclude a surviving TLSRoute from the
	// owner inventory, even though this Access no longer selects Gateway.
	legacy := desiredTLSRoute(sa, sa.Spec.Ports[0], "controller", "gateway.example")
	legacy.SetLabels(map[string]string{"internal.skupper.io/securedaccess": sa.Name})
	_, err := h.clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace("test").Create(context.Background(), legacy, metav1.CreateOptions{})
	assert.NilError(t, err)
	assert.NilError(t, h.clients.Skupper.SkupperV2alpha1().SecuredAccesses("test").Delete(context.Background(), "edge", metav1.DeleteOptions{}))
	h.observe()
	// A failed removal is retryable and does not erase discovery state.
	k := h.clients.Kube.(*k8sfake.Clientset)
	denied := errors.New("deletion denied")
	k.PrependReactor("delete", "services", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, denied })
	assert.Assert(t, errors.Is(h.run("test/edge"), denied))
	k.ReactionChain = k.ReactionChain[1:]
	h = newHarness(t, h.clients)
	h.observe()
	p := watchers.NewEventProcessor("AccessTest", h.clients)
	h.m.config.EnabledAccessTypes = []string{"local"}
	h.m.watcher.processor = p
	// No definition remains. The resource callback still routes its owner scope.
	assert.NilError(t, h.m.watcher.resourceChanged("test/edge"))
	p.TestProcessAll()
	svcs, err := h.clients.Kube.CoreV1().Services("test").List(context.Background(), metav1.ListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(svcs.Items), 0)
	routes, err := h.clients.Dynamic.Resource(resource.TlsRouteResource()).Namespace("test").List(context.Background(), metav1.ListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(routes.Items), 0)
	_, err = h.clients.Skupper.SkupperV2alpha1().Certificates("test").Get(context.Background(), "edge", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(err))
}

func TestUncontrolledScopeDoesNotReadOrWrite(t *testing.T) {
	h := newHarness(t, nil)
	h.m.context.(*testContext).excluded = "test"
	// A nil watcher would panic if the namespace gate occurred after snapshot.
	h.m.watcher = nil
	assert.NilError(t, h.m.Handle(watchers.ResourceChange{Key: "test/edge"}))
	assert.NilError(t, h.m.certificates.Handle(watchers.ResourceChange{Key: "test/edge"}))
}

func TestControlAndPolicyWakeups(t *testing.T) {
	h := newHarness(t, nil)
	h.put(testAccess("local"))
	h.m.config.EnabledAccessTypes = []string{"local"}
	c := h.m.context.(*testContext)
	c.excluded = "test"
	p := watchers.NewEventProcessor("AccessTest", h.clients)
	h.m.watcher.processor = p
	h.m.watcher.EnqueueNamespace("test")
	p.TestProcessAll()
	_, err := h.clients.Kube.CoreV1().Services("test").Get(context.Background(), "edge", metav1.GetOptions{})
	assert.Assert(t, apierrors.IsNotFound(err))
	c.excluded = ""
	h.m.watcher.EnqueueNamespace("test")
	p.TestProcessAll()
	h.observe()
	assert.Assert(t, h.status("edge").IsReady())
	assert.NilError(t, c.policy.Update("controller/template", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "controller", Labels: map[string]string{"skupper.io/label-template": "true", "policy": "updated"}}}))
	h.m.watcher.EnqueueNamespace("")
	p.TestProcessAll()
	svc, err := h.clients.Kube.CoreV1().Services("test").Get(context.Background(), "edge", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, svc.Labels["policy"], "updated")
	h.observe()
	h.noWrites("edge")
}
