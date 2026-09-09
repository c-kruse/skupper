package controller

import (
	"flag"
	"reflect"
	"testing"

	fakeclient "github.com/skupperproject/skupper/internal/kube/client/fake"
	routerstatus "github.com/skupperproject/skupper/pkg/skrouter/status"
	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRouterStatusHandlerMalformedDocumentClearsGroup(t *testing.T) {
	config, err := BoundConfig(flag.NewFlagSet("test", flag.ContinueOnError))
	assert.NilError(t, err)
	clients, err := fakeclient.NewFakeClient("test", nil, nil, "")
	assert.NilError(t, err)
	controller, err := NewController(clients, config)
	assert.NilError(t, err)

	encoded, err := routerstatus.Encode(&routerstatus.Document{Version: 1})
	assert.NilError(t, err)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "group1-status", Namespace: "test", Labels: map[string]string{routerstatus.GroupLabel: "group1"}},
		BinaryData: map[string][]byte{routerstatus.DataKey: encoded},
	}
	assert.NilError(t, controller.routerStatusUpdate("test/group1-status", cm))
	statusMap := reflect.ValueOf(controller.getSite("test")).Elem().FieldByName("routerStatus")
	assert.Equal(t, statusMap.Len(), 1)

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
