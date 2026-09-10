package controller

import (
	"context"
	"flag"
	"reflect"
	"testing"

	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	routerstatus "github.com/skupperproject/skupper/internal/routerstatus"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestLocalOnlyListenersWithoutSite(t *testing.T) {
	config, err := BoundConfig(flag.NewFlagSet("test", flag.ContinueOnError))
	assert.NilError(t, err)
	config.LegacyNetworkStatus = false
	listener := &skupperv2alpha1.Listener{ObjectMeta: metav1.ObjectMeta{Name: "listener", Namespace: "test"}}
	mkl := &skupperv2alpha1.MultiKeyListener{ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "test"}}
	clients, err := fakeclient.NewFakeClient("test", nil, []runtime.Object{listener, mkl}, "")
	assert.NilError(t, err)
	controller, err := NewController(clients, config)
	assert.NilError(t, err)
	assert.NilError(t, controller.checkListener("test/listener", listener))
	assert.NilError(t, controller.checkMultiKeyListener("test/multi", mkl))
	api := clients.GetSkupperClient().SkupperV2alpha1()
	gotListener, err := api.Listeners("test").Get(context.Background(), "listener", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Assert(t, meta.IsStatusConditionFalse(gotListener.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED))
	gotMulti, err := api.MultiKeyListeners("test").Get(context.Background(), "multi", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Assert(t, meta.IsStatusConditionFalse(gotMulti.Status.Conditions, skupperv2alpha1.CONDITION_TYPE_CONFIGURED))
	assert.NilError(t, controller.checkListener("test/listener", nil))
	assert.NilError(t, controller.checkMultiKeyListener("test/multi", nil))
}

func TestRouterStatusHandlerRemovesOnlyInvalidReplica(t *testing.T) {
	config, err := BoundConfig(flag.NewFlagSet("test", flag.ContinueOnError))
	assert.NilError(t, err)
	clients, err := fakeclient.NewFakeClient("test", nil, nil, "")
	assert.NilError(t, err)
	controller, err := NewController(clients, config)
	assert.NilError(t, err)

	encoded, err := routerstatus.Encode(&routerstatus.Document{Version: 1, Group: "group1", Router: routerstatus.Router{Hostname: "group1", PodUID: "pod-uid"}})
	assert.NilError(t, err)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "group1-status", Namespace: "test", Labels: map[string]string{routerstatus.GroupLabel: "group1"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "group1", UID: "pod-uid"}}},
		BinaryData: map[string][]byte{routerstatus.DataKey: encoded},
	}
	assert.NilError(t, controller.routerStatusUpdate("test/group1-status", cm))
	statusMap := reflect.ValueOf(controller.getSite("test")).Elem().FieldByName("routerStatus")
	assert.Equal(t, statusMap.Len(), 1)

	other := cm.DeepCopy()
	other.Name = "pod2-status"
	other.OwnerReferences[0].Name = "pod2"
	other.OwnerReferences[0].UID = "pod2-uid"
	other.BinaryData[routerstatus.DataKey], err = routerstatus.Encode(&routerstatus.Document{Version: 1, Group: "group1", Router: routerstatus.Router{Hostname: "pod2", PodUID: "pod2-uid"}})
	assert.NilError(t, err)
	assert.NilError(t, controller.routerStatusUpdate("test/pod2-status", other))
	assert.Equal(t, statusMap.Len(), 2, "same-group replicas are independent")

	for _, invalidate := range []func(*corev1.ConfigMap){
		func(cm *corev1.ConfigMap) { cm.OwnerReferences[0].UID = "replacement" },
		func(cm *corev1.ConfigMap) { cm.OwnerReferences[0].Kind = "Deployment" },
		func(cm *corev1.ConfigMap) { cm.Labels[routerstatus.GroupLabel] = "other-group" },
		func(cm *corev1.ConfigMap) { cm.BinaryData[routerstatus.DataKey] = []byte("not gzip") },
	} {
		invalid := other.DeepCopy()
		invalidate(invalid)
		assert.NilError(t, controller.routerStatusUpdate("test/pod2-status", invalid))
		assert.Equal(t, statusMap.Len(), 1)
		assert.Assert(t, statusMap.MapIndex(reflect.ValueOf("group1")).IsValid())
		assert.NilError(t, controller.routerStatusUpdate("test/pod2-status", other))
	}
	assert.NilError(t, controller.routerStatusUpdate("test/pod2-status", nil))
	assert.Equal(t, statusMap.Len(), 1)
	assert.Assert(t, statusMap.MapIndex(reflect.ValueOf("group1")).IsValid())

	cm.BinaryData[routerstatus.DataKey] = []byte("not gzip")
	assert.NilError(t, controller.routerStatusUpdate("test/group1-status", cm))
	assert.Equal(t, statusMap.Len(), 0, "a malformed replacement must remove stale observations")

	// A malformed, previously unseen group is ignored without manufacturing state.
	cm.Name = "group2-status"
	cm.Labels[routerstatus.GroupLabel] = "group2"
	assert.NilError(t, controller.routerStatusUpdate("test/group2-status", cm))
	assert.Equal(t, statusMap.Len(), 0)
}

func TestLegacyStatusFlagDisablesLegacyWatcherOnly(t *testing.T) {
	config, err := BoundConfig(flag.NewFlagSet("test", flag.ContinueOnError))
	assert.NilError(t, err)
	config.LegacyNetworkStatus = false
	clients, err := fakeclient.NewFakeClient("test", nil, nil, "")
	assert.NilError(t, err)
	controller, err := NewController(clients, config)
	assert.NilError(t, err)
	assert.Assert(t, controller.networkStatusWatcher == nil)
	assert.Assert(t, controller.routerStatusWatcher != nil)
}
