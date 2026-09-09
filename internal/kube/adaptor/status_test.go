package adaptor

import (
	"context"
	"errors"
	"testing"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	kubeqdr "github.com/skupperproject/skupper/internal/kube/qdr"
	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routerstatus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testNamespace = "test"

func TestConfigUpdatedReadsQdrSnapshot(t *testing.T) {
	for _, threshold := range []int{0, 1} {
		publisher, _ := testPublisher()
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "revision-a"}}
		config := qdr.InitialConfig("router-a", "site-a", "test", true, 3)
		config.AddConnector(qdr.Connector{Name: "west", Role: "edge", Host: "west", Port: "4567"})
		writer := kubeqdr.ConfigMapWriter{CompressionThreshold: threshold}
		if err := writer.WriteConfigMap(&config, cm); err != nil {
			t.Fatal(err)
		}
		if err := kubeqdr.WriteAdaptorConfigToConfigMap(qdr.AdaptorConfig{Version: 1, AddressPrefixes: []qdr.AddressPrefix{{Prefix: "backend."}}}, cm); err != nil {
			t.Fatal(err)
		}
		publisher.ConfigUpdated(cm, errors.New("sync failed"))
		if publisher.desired == nil || publisher.desired.Connectors["west"].Host != "west" || publisher.desired.Metadata.Mode != qdr.ModeEdge {
			t.Fatalf("threshold %d: desired = %#v", threshold, publisher.desired)
		}
		if publisher.applied != (status.Applied{ResourceVersion: "revision-a", Error: "sync failed"}) || len(publisher.adaptor.AddressPrefixes) != 1 || publisher.adaptor.AddressPrefixes[0].Prefix != "backend." {
			t.Fatalf("applied/adaptor = %#v / %#v", publisher.applied, publisher.adaptor)
		}
		select {
		case <-publisher.next:
		default:
			t.Fatal("configuration update did not notify publisher")
		}
		publisher.ConfigUpdated(cm, nil)
		if publisher.applied.Error != "" {
			t.Fatal("successful sync retained previous error")
		}
	}
}

func testPublisher(objects ...runtime.Object) (*StatusPublisher, *k8sfake.Clientset) {
	client := k8sfake.NewSimpleClientset(objects...)
	return NewStatusPublisher(&internalclient.KubeClient{Namespace: testNamespace, Kube: client}), client
}

func testDeployment(group string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: group, Namespace: testNamespace, UID: types.UID("deployment-uid"),
	}}
}

func TestPublishCreatesGzipConfigMapWithOwnershipAndLabels(t *testing.T) {
	doc := &status.Document{Version: 1, Group: "router-group", Router: status.Router{ID: "router-a"}}
	publisher, client := testPublisher(testDeployment(doc.Group))

	if err := publisher.publish(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), doc.Group+"-status", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := status.Decode(cm.BinaryData[status.DataKey])
	if err != nil {
		t.Fatalf("status data is not valid gzip: %v", err)
	}
	if decoded.Group != doc.Group || decoded.Router.ID != doc.Router.ID {
		t.Fatalf("unexpected decoded status: %#v", decoded)
	}
	if value, ok := cm.Labels[status.ConfigMapLabel]; !ok || value != "" {
		t.Fatalf("missing status label: %#v", cm.Labels)
	}
	if cm.Labels[status.GroupLabel] != doc.Group {
		t.Fatalf("unexpected group label: %#v", cm.Labels)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != types.UID("deployment-uid") || cm.OwnerReferences[0].Kind != "Deployment" {
		t.Fatalf("unexpected owner references: %#v", cm.OwnerReferences)
	}
}

func TestPublishSuppressesNoOpUpdate(t *testing.T) {
	doc := &status.Document{Version: 1, Group: "router-group"}
	payload, err := status.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: doc.Group + "-status", Namespace: testNamespace}, BinaryData: map[string][]byte{status.DataKey: payload}}
	publisher, client := testPublisher(testDeployment(doc.Group), existing)
	client.ClearActions()

	if err := publisher.publish(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.Matches("update", "configmaps") || action.Matches("create", "configmaps") {
			t.Fatalf("unchanged status caused API write: %#v", action)
		}
	}
}

func TestPublishRetriesConflict(t *testing.T) {
	doc := &status.Document{Version: 1, Group: "router-group", Applied: status.Applied{ResourceVersion: "2"}}
	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: doc.Group + "-status", Namespace: testNamespace}, BinaryData: map[string][]byte{status.DataKey: []byte("old")}}
	publisher, client := testPublisher(testDeployment(doc.Group), existing)
	updates := 0
	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, existing.Name, errors.New("conflict"))
		}
		return false, nil, nil
	})

	if err := publisher.publish(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Fatalf("got %d update attempts, want 2", updates)
	}
}

func TestPublishCanceledContextStopsWrites(t *testing.T) {
	doc := &status.Document{Version: 1, Group: "router-group"}
	publisher, client := testPublisher(testDeployment(doc.Group))
	client.ClearActions()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := publisher.publish(ctx, doc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("canceled publish made API calls: %#v", client.Actions())
	}
}
