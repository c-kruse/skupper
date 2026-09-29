package controllerruntime

import (
	"context"
	"testing"
	"time"

	protocol "github.com/skupperproject/skupper/internal/routercontrol"
)

type refreshFunc func(protocol.SessionKey, string, string, string) error

func (f refreshFunc) RequestRefresh(key protocol.SessionKey, session, request, scope string) error {
	return f(key, session, request, scope)
}

func testObservationCache() (*observationCache, protocol.SessionKey) {
	c := newObservationCache(nil)
	key := protocol.SessionKey{
		Target:   protocol.TargetIdentity{NamespaceUID: "ns-uid", SiteUID: "site-uid", RouterGroup: "skupper-router"},
		Identity: protocol.SessionIdentity{PodUID: "old-pod", ServiceAccountUID: "sa-uid"},
	}
	c.NoteNamespace("ns-uid", "tenant")
	c.Connected(key, "session-1", protocol.Hello{RouterIncarnation: "router-1"})
	return c, key
}

func TestObservationFreshnessRequiresTimelyScopedRefresh(t *testing.T) {
	c, key := testObservationCache()
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	before := c.Snapshot("tenant", now)[key.Target][0]
	if before.Scopes[protocol.ObservationScopeResources].Fresh {
		t.Fatal("a newly connected router was treated as observed")
	}
	requests := map[string]string{}
	c.refresh(refreshFunc(func(_ protocol.SessionKey, session, request, scope string) error {
		if session != "session-1" {
			t.Fatalf("refresh was sent to %q", session)
		}
		requests[scope] = request
		return nil
	}))
	observation := protocol.ObservationSnapshot{
		SessionID: "session-1", RouterIncarnation: "router-1", Scope: protocol.ObservationScopeResources,
		SampleSequence: 1, Knowledge: protocol.KnowledgeComplete,
		RefreshRequestID: requests[protocol.ObservationScopeResources],
	}
	now = now.Add(2 * time.Second)
	if err := c.Observation(context.Background(), key, observation); err != nil {
		t.Fatal(err)
	}
	current := c.Snapshot("tenant", now)[key.Target][0]
	resources := current.Scopes[protocol.ObservationScopeResources]
	if !resources.Fresh || resources.Snapshot.Knowledge != protocol.KnowledgeComplete || len(resources.Snapshot.Resources) != 0 {
		t.Fatal("explicit complete empty reply was not retained as known empty")
	}
	if current.Scopes[protocol.ObservationScopeAddresses].Fresh {
		t.Fatal("resource refresh incorrectly freshened the address scope")
	}
	// A later unsolicited sample must not extend the original request's TTL.
	now = time.Unix(1029, 0)
	observation.SampleSequence = 2
	observation.RefreshRequestID = ""
	if err := c.Observation(context.Background(), key, observation); err != nil {
		t.Fatal(err)
	}
	if !c.Snapshot("tenant", now)[key.Target][0].Scopes[protocol.ObservationScopeResources].Fresh {
		t.Fatal("facts expired before the request-anchored deadline")
	}
	now = time.Unix(1030, 0)
	if c.Snapshot("tenant", now)[key.Target][0].Scopes[protocol.ObservationScopeResources].Fresh {
		t.Fatal("unsolicited sample or response receipt time extended freshness")
	}
	// Expiry itself must wake reconciliation; no informer event is needed.
	invalidated := false
	c.invalidate = func(names ...string) { invalidated = len(names) == 1 && names[0] == "tenant" }
	c.refresh(refreshFunc(func(protocol.SessionKey, string, string, string) error { return nil }))
	if !invalidated {
		t.Fatal("freshness expiry did not invalidate namespace status")
	}
}

func TestLateRefreshCannotReplaceFactsAndUnknownIsNotEmpty(t *testing.T) {
	c, key := testObservationCache()
	now := time.Unix(2000, 0)
	c.now = func() time.Time { return now }
	requests := map[string]string{}
	c.refresh(refreshFunc(func(_ protocol.SessionKey, _, request, scope string) error {
		requests[scope] = request
		return nil
	}))
	observation := protocol.ObservationSnapshot{
		SessionID: "session-1", RouterIncarnation: "router-1", Scope: protocol.ObservationScopeAddresses,
		SampleSequence: 1, Knowledge: protocol.KnowledgePartial,
		Addresses: []protocol.LocalAddressObservation{{RoutingKey: "orders", SubscriberCount: 2, Reachable: true}},
	}
	if err := c.Observation(context.Background(), key, observation); err != nil {
		t.Fatal(err)
	}
	now = now.Add(observationDeadline)
	late := observation
	late.RefreshRequestID = requests[protocol.ObservationScopeAddresses]
	late.SampleSequence = 9
	late.Knowledge = protocol.KnowledgeComplete
	late.Addresses = nil
	if err := c.Observation(context.Background(), key, late); err != nil {
		t.Fatal(err)
	}
	scope := c.Snapshot("tenant", now)[key.Target][0].Scopes[protocol.ObservationScopeAddresses]
	if scope.Fresh || len(scope.Snapshot.Addresses) != 1 || scope.Snapshot.SampleSequence != 1 {
		t.Fatal("late empty response replaced newer usable facts or asserted freshness")
	}
	unknown := observation
	unknown.SampleSequence = 2
	unknown.Knowledge = protocol.KnowledgeUnknown
	unknown.Reason = "management query failed"
	unknown.Addresses = nil
	if err := c.Observation(context.Background(), key, unknown); err != nil {
		t.Fatal(err)
	}
	scope = c.Snapshot("tenant", now)[key.Target][0].Scopes[protocol.ObservationScopeAddresses]
	if scope.Fresh || scope.Snapshot.Knowledge != protocol.KnowledgeUnknown {
		t.Fatal("failed management query became a known-empty observation")
	}
}

func TestRolloutPodsCoexistAndReplacedSessionsAreFenced(t *testing.T) {
	c, key := testObservationCache()
	newPod := key
	newPod.Identity.PodUID = "new-pod"
	c.Connected(newPod, "session-2", protocol.Hello{RouterIncarnation: "router-2"})
	if len(c.Snapshot("tenant", time.Now())[key.Target]) != 2 {
		t.Fatal("new rollout Pod evicted the old Pod's distinct realization")
	}
	c.Connected(key, "session-3", protocol.Hello{RouterIncarnation: "router-3"})
	c.Disconnected(key, "session-1")
	c.Accepted(key, protocol.Accepted{SessionID: "session-1", Digest: "old", Sequence: 99})
	if err := c.Application(context.Background(), key, protocol.ApplicationReport{SessionID: "session-1", RouterIncarnation: "router-1"}); err == nil {
		t.Fatal("old session report was accepted after replacement")
	}
	if len(c.Snapshot("tenant", time.Now())[key.Target]) != 2 {
		t.Fatal("old session disconnect deleted the replacement")
	}
	c.Disconnected(key, "session-3")
	remaining := c.Snapshot("tenant", time.Now())[key.Target]
	if len(remaining) != 1 || remaining[0].Key.Identity.PodUID != "new-pod" {
		t.Fatal("disconnect affected another Pod")
	}
	if len(c.Snapshot("other-tenant", time.Now())) != 0 {
		t.Fatal("snapshot leaked another namespace's observations")
	}
}

func TestAcceptedAppliedAndSnapshotsAreIndependent(t *testing.T) {
	c, key := testObservationCache()
	c.Accepted(key, protocol.Accepted{SessionID: "session-1", Digest: "current", Sequence: 3})
	if c.Snapshot("tenant", time.Now())[key.Target][0].Application != nil {
		t.Fatal("acceptance was treated as application")
	}
	stale := protocol.ApplicationReport{SessionID: "session-1", RouterIncarnation: "router-1", IntentDigest: "old", Sequence: 2}
	if err := c.Application(context.Background(), key, stale); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot("tenant", time.Now())[key.Target][0].Application != nil {
		t.Fatal("old application satisfied newer accepted intent")
	}
	report := protocol.ApplicationReport{
		SessionID: "session-1", RouterIncarnation: "router-1", IntentDigest: "current", Sequence: 3,
		State: protocol.ApplicationApplied, RealizationID: "realized-1",
		Credentials: []protocol.CredentialRevision{{BindingID: "credential", Revision: "version-1"}},
	}
	if err := c.Application(context.Background(), key, report); err != nil {
		t.Fatal(err)
	}
	report.Credentials[0].Revision = "mutated-input"
	snapshot := c.Snapshot("tenant", time.Now())[key.Target][0]
	if snapshot.Application.Credentials[0].Revision != "version-1" {
		t.Fatal("cache retained mutable report slices")
	}
	snapshot.Application.Credentials[0].Revision = "mutated-snapshot"
	if c.Snapshot("tenant", time.Now())[key.Target][0].Application.Credentials[0].Revision != "version-1" {
		t.Fatal("snapshot exposed the cache's mutable slices")
	}
}
