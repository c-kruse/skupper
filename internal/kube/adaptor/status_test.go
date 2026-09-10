package adaptor

import (
	"context"
	"errors"
	"testing"
	"time"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	kubeqdr "github.com/skupperproject/skupper/internal/kube/qdr"
	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routerstatus"
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

func TestStatusPublisherRateLimit(t *testing.T) {
	publisher, _ := testPublisher()
	now := time.Now()
	if !publisher.limiter.AllowN(now, 3) {
		t.Fatal("startup burst must allow three immediate attempts")
	}
	if publisher.limiter.AllowN(now, 1) {
		t.Fatal("startup burst must not allow a fourth attempt")
	}
	if publisher.limiter.AllowN(now.Add(5*time.Second-time.Millisecond), 1) {
		t.Fatal("exhausted budget must wait five seconds for another attempt")
	}
	if !publisher.limiter.AllowN(now.Add(5*time.Second), 1) {
		t.Fatal("budget must refill after five seconds")
	}
	if publisher.limiter.AllowN(now.Add(5*time.Second), 1) {
		t.Fatal("five seconds must replenish only one attempt")
	}
	if !publisher.limiter.AllowN(now.Add(10*time.Second), 1) {
		t.Fatal("sustained attempts must remain available every five seconds")
	}
}

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
	publisher := NewStatusPublisher(&internalclient.KubeClient{Namespace: testNamespace, Kube: client})
	publisher.podUID = "pod-uid"
	return publisher, client
}

func testPod(name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: testNamespace, UID: types.UID("pod-uid"),
	}}
}

func TestPublishCreatesGzipConfigMapWithOwnershipAndLabels(t *testing.T) {
	doc := &status.Document{Version: 1, Group: "router-group", Router: status.Router{ID: "router-a", Hostname: "pod-a"}}
	publisher, client := testPublisher(testPod(doc.Router.Hostname))

	if err := publisher.publish(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	cm, err := client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), doc.Router.Hostname+"-status", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := status.Decode(cm.BinaryData[status.DataKey])
	if err != nil {
		t.Fatalf("status data is not valid gzip: %v", err)
	}
	if decoded.Group != doc.Group || decoded.Router.ID != doc.Router.ID || decoded.Router.PodUID != "pod-uid" {
		t.Fatalf("unexpected decoded status: %#v", decoded)
	}
	if value, ok := cm.Labels[status.ConfigMapLabel]; !ok || value != "" {
		t.Fatalf("missing status label: %#v", cm.Labels)
	}
	if cm.Labels[status.GroupLabel] != doc.Group {
		t.Fatalf("unexpected group label: %#v", cm.Labels)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != types.UID("pod-uid") || cm.OwnerReferences[0].Kind != "Pod" {
		t.Fatalf("unexpected owner references: %#v", cm.OwnerReferences)
	}
}

func TestPublishSuppressesNoOpUpdate(t *testing.T) {
	doc := &status.Document{Version: 1, Group: "router-group", Router: status.Router{Hostname: "pod-a", PodUID: "pod-uid"}}
	payload, err := status.Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: doc.Router.Hostname + "-status", Namespace: testNamespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: "pod-a", UID: "pod-uid"}}}, BinaryData: map[string][]byte{status.DataKey: payload}}
	publisher, client := testPublisher(testPod(doc.Router.Hostname), existing)
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
	doc := &status.Document{Version: 1, Group: "router-group", Router: status.Router{Hostname: "pod-a"}, Applied: status.Applied{ResourceVersion: "2"}}
	existing := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: doc.Router.Hostname + "-status", Namespace: testNamespace}, BinaryData: map[string][]byte{status.DataKey: []byte("old")}}
	publisher, client := testPublisher(testPod(doc.Router.Hostname), existing)
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
	publisher, client := testPublisher()
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

func TestPublishReplicasDoNotOverwriteEachOther(t *testing.T) {
	podA, podB := testPod("pod-a"), testPod("pod-b")
	podB.UID = "pod-b-uid"
	publisher, client := testPublisher(podA, podB)
	docA := &status.Document{Version: 1, Group: "same-group", Router: status.Router{ID: "router-a", Hostname: "pod-a"}}
	docB := &status.Document{Version: 1, Group: "same-group", Router: status.Router{ID: "router-b", Hostname: "pod-b"}}
	if err := publisher.publish(context.Background(), docA); err != nil {
		t.Fatal(err)
	}
	publisherB := NewStatusPublisher(publisher.cli)
	publisherB.podUID = string(podB.UID)
	if err := publisherB.publish(context.Background(), docB); err != nil {
		t.Fatal(err)
	}
	docA.Applied.ResourceVersion = "new-revision"
	if err := publisher.publish(context.Background(), docA); err != nil {
		t.Fatal(err)
	}
	list, err := client.CoreV1().ConfigMaps(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("got %d documents, want two", len(list.Items))
	}
	for _, cm := range list.Items {
		doc, err := status.Decode(cm.BinaryData[status.DataKey])
		if err != nil {
			t.Fatal(err)
		}
		if cm.Name != doc.Router.Hostname+"-status" || cm.OwnerReferences[0].Name != doc.Router.Hostname || string(cm.OwnerReferences[0].UID) != doc.Router.PodUID {
			t.Fatalf("incorrect pod ownership: %#v / %#v", cm.ObjectMeta, doc.Router)
		}
		if doc.Router.Hostname == "pod-b" && doc.Applied.ResourceVersion != "" {
			t.Fatal("pod-a overwrote pod-b")
		}
	}
}

func TestPublishExcludesTerminatingOrReplacedPod(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		pod := testPod("pod-a")
		if replaced {
			pod.UID = "replacement"
		} else {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
		}
		publisher, client := testPublisher(pod)
		if err := publisher.publish(context.Background(), &status.Document{Version: 1, Router: status.Router{Hostname: pod.Name}}); err != nil {
			t.Fatal(err)
		}
		for _, action := range client.Actions() {
			if action.Matches("create", "configmaps") || action.Matches("update", "configmaps") {
				t.Fatal("ineligible pod published status")
			}
		}
	}
}
