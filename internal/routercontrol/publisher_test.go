package routercontrol

import "testing"

func TestPublishedIntentsAreNamespaceScopedIsolatedState(t *testing.T) {
	publisher := NewPublisher()
	intent := testIntent()
	digest, err := publisher.Publish(intent)
	if err != nil {
		t.Fatal(err)
	}
	other := testIntent()
	other.Target.NamespaceUID = "other-namespace"
	other.ServiceConnectors[0].Target = other.Target
	if _, err := publisher.Publish(other); err != nil {
		t.Fatal(err)
	}

	snapshot := publisher.PublishedIntents(intent.Target.NamespaceUID)
	if len(snapshot) != 1 || snapshot[intent.Target] != (Publication{Digest: digest, Available: true, Revision: 1}) {
		t.Fatalf("unexpected publication snapshot: %#v", snapshot)
	}
	delete(snapshot, intent.Target)
	if current := publisher.PublishedIntents(intent.Target.NamespaceUID); len(current) != 1 {
		t.Fatal("caller mutated publisher state through snapshot")
	}
	snapshot = publisher.PublishedIntents(intent.Target.NamespaceUID)
	publisher.SetUnavailable(intent.Target)
	if !snapshot[intent.Target].Available {
		t.Fatal("later publication mutated a collected snapshot")
	}
	if current := publisher.PublishedIntents(intent.Target.NamespaceUID)[intent.Target]; current != (Publication{Revision: 2}) {
		t.Fatalf("unavailable target retained stale available evidence: %#v", current)
	}
	if current := NewPublisher().PublishedIntents(intent.Target.NamespaceUID); len(current) != 0 {
		t.Fatalf("new leader inherited earlier publication evidence: %#v", current)
	}
}
