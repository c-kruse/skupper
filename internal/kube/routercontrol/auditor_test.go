package routercontrol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

type auditorFixture struct {
	t             *testing.T
	client        *fake.Clientset
	installation  *Installation
	leaf          *x509.Certificate
	revocations   *SessionRevocations
	authenticator *Authenticator
}

func newAuditorFixture(t *testing.T) *auditorFixture {
	t.Helper()
	base := newSessionFixture(t)
	revocations := NewSessionRevocations()
	revocations.freshness = 5 * time.Second
	revocations.interval = time.Minute
	fixture := &auditorFixture{t: t, client: base.client, installation: base.install, leaf: base.leaf, revocations: revocations}
	fixture.authenticator = &Authenticator{
		Kube: base.client, Installation: base.install, Gate: base.gate, Revocations: revocations,
		Authorize: fixture.authorizeAssignment,
	}
	return fixture
}

func (f *auditorFixture) authorizeAssignment(ctx context.Context, identity Identity) error {
	for _, read := range []struct{ key, name string }{
		{"configmaps/" + identity.Namespace + "/skupper", "skupper"},
		{"sites/" + identity.Namespace + "/" + identity.SiteName, "site-proof"},
		{"configmaps/" + identity.Namespace + "/skupper-controller-allocations", "skupper-controller-allocations"},
	} {
		value, err := AuthorizationRead(ctx, read.key, func(ctx context.Context) (*corev1.ConfigMap, error) {
			return f.client.CoreV1().ConfigMaps(identity.Namespace).Get(ctx, read.name, metav1.GetOptions{})
		})
		if err != nil {
			return AuthorizationUnavailable(err)
		}
		if value.Data["namespaceUID"] != string(identity.NamespaceUID) || value.Data["siteUID"] != string(identity.SiteUID) {
			return ErrUnauthorized
		}
	}
	return nil
}

func (f *auditorFixture) addIdentity(namespace, group, suffix string) (Identity, tls.ConnectionState) {
	f.t.Helper()
	controller := true
	identity := Identity{
		Installation: f.installation.Name, Namespace: namespace, NamespaceUID: types.UID(namespace + "-uid"), SiteName: "west", SiteUID: types.UID(namespace + "-site-uid"), Group: group,
		PodName: group + "-pod-" + suffix, PodUID: types.UID(group + "-pod-uid-" + suffix), ServiceAccount: "router-sa", ServiceAccountUID: types.UID(namespace + "-sa-uid"),
	}
	ctx := context.Background()
	create := func(err error) {
		if err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := f.client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err != nil {
		_, err = f.client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: identity.NamespaceUID}}, metav1.CreateOptions{})
		create(err)
		_, err = f.client.CoreV1().ServiceAccounts(namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: identity.ServiceAccount, Namespace: namespace, UID: identity.ServiceAccountUID}}, metav1.CreateOptions{})
		create(err)
		for _, name := range []string{"skupper", "site-proof", "skupper-controller-allocations"} {
			_, err = f.client.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Data: map[string]string{"namespaceUID": string(identity.NamespaceUID), "siteUID": string(identity.SiteUID)}}, metav1.CreateOptions{})
			create(err)
		}
	}
	deploymentUID := types.UID(namespace + "-" + group + "-deployment-uid")
	replicaSetName := group + "-rs"
	replicaSetUID := types.UID(namespace + "-" + group + "-rs-uid")
	if _, err := f.client.AppsV1().Deployments(namespace).Get(ctx, group, metav1.GetOptions{}); err != nil {
		_, err = f.client.AppsV1().Deployments(namespace).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: group, Namespace: namespace, UID: deploymentUID, OwnerReferences: []metav1.OwnerReference{{APIVersion: siteAPIVer, Kind: "Site", Name: identity.SiteName, UID: identity.SiteUID, Controller: &controller}}}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{GroupLabel: group}}}}}, metav1.CreateOptions{})
		create(err)
		_, err = f.client.AppsV1().ReplicaSets(namespace).Create(ctx, &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: replicaSetName, Namespace: namespace, UID: replicaSetUID, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: group, UID: deploymentUID, Controller: &controller}}}}, metav1.CreateOptions{})
		create(err)
	}
	_, err := f.client.CoreV1().Pods(namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: identity.PodName, Namespace: namespace, UID: identity.PodUID, Labels: map[string]string{GroupLabel: group}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSetName, UID: replicaSetUID, Controller: &controller}}}, Spec: corev1.PodSpec{ServiceAccountName: identity.ServiceAccount}}, metav1.CreateOptions{})
	create(err)
	uri, err := identityURI(identity)
	create(err)
	leaf := *f.leaf
	leaf.NotBefore = time.Now().Add(-time.Minute)
	leaf.NotAfter = time.Now().Add(time.Hour)
	leaf.URIs = []*url.URL{uri}
	return identity, tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{&leaf, f.installation.ClientCA}}}
}

func (f *auditorFixture) session(state tls.ConnectionState) *Session {
	f.t.Helper()
	session, err := f.authenticator.Session(context.Background(), state)
	if err != nil {
		f.t.Fatal(err)
	}
	return session
}

func (f *auditorFixture) auditRound() int {
	f.t.Helper()
	now := f.revocations.now()
	f.revocations.mu.Lock()
	for _, state := range f.revocations.sessions {
		state.nextAudit = now
	}
	f.revocations.mu.Unlock()
	before := len(f.client.Actions())
	for _, batch := range f.revocations.due(now) {
		f.revocations.complete(batch, f.authenticator.audit(context.Background(), batch))
	}
	return len(f.client.Actions()) - before
}

func TestAuditSpreadCoversTheWholeMinute(t *testing.T) {
	var buckets [6]int
	for i := 0; i < 1024; i++ {
		delay := auditSpread(fmt.Sprintf("tenant-%04d", i), time.Minute)
		if delay < 0 || delay >= time.Minute {
			t.Fatalf("audit delay %v is outside the interval", delay)
		}
		buckets[delay/(10*time.Second)]++
	}
	for bucket, count := range buckets {
		if count == 0 {
			t.Fatalf("no audits scheduled in ten-second bucket %d: %v", bucket, buckets)
		}
	}
}

func TestWatchErrorsCannotBypassAuditRetrySpacing(t *testing.T) {
	fixture := newAuditorFixture(t)
	fixture.revocations.freshness = DefaultAuthorizationFreshness
	_, state := fixture.addIdentity("tenant", "skupper-router", "one")
	session := fixture.session(state)
	defer session.Close()
	now := time.Now()
	fixture.revocations.now = func() time.Time { return now }
	fixture.revocations.AuthorizationWatchFailed(errors.New("watch unavailable"))
	batches := fixture.revocations.due(now)
	if len(batches) != 1 {
		t.Fatalf("initial early audit batches = %d, want 1", len(batches))
	}
	fixture.revocations.complete(batches[0], map[Identity]error{session.Identity: AuthorizationUnavailable(errors.New("429"))})
	for i := 0; i < 4; i++ {
		now = now.Add(time.Second)
		fixture.revocations.AuthorizationWatchFailed(errors.New("watch still unavailable"))
		if batches := fixture.revocations.due(now); len(batches) != 0 {
			t.Fatalf("watch error bypassed the five-second retry delay after %ds", i+1)
		}
	}
	now = now.Add(time.Second)
	if batches := fixture.revocations.due(now); len(batches) != 1 {
		t.Fatalf("retry batches at the five-second boundary = %d, want 1", len(batches))
	}
}

func TestQueuedAuditSkipsClosedAndExpiredSessions(t *testing.T) {
	for _, reason := range []string{"closed", "expired"} {
		t.Run(reason, func(t *testing.T) {
			fixture := newAuditorFixture(t)
			fixture.revocations.freshness = DefaultAuthorizationFreshness
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()) }
			fixture.authenticator.Now = now
			fixture.revocations.now = now
			_, state := fixture.addIdentity("tenant", "skupper-router", "one")
			session := fixture.session(state)
			defer session.Close()
			fixture.revocations.AuthorizationWatchFailed(errors.New("watch unavailable"))
			batch := fixture.revocations.due(now())[0]
			if reason == "closed" {
				session.Close()
			} else {
				clock.Add(int64(DefaultAuthorizationFreshness))
			}
			fixture.client.ClearActions()
			results := fixture.authenticator.audit(context.Background(), batch)
			if len(results) != 0 || len(fixture.client.Actions()) != 0 {
				t.Fatalf("queued audit of %s session produced %d results and %d API requests", reason, len(results), len(fixture.client.Actions()))
			}
		})
	}
}

func TestAuditorUsesRealDistinctIdentityReadsAcrossPeriods(t *testing.T) {
	fixture := newAuditorFixture(t)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(duration time.Duration) { clock.Add(int64(duration)) }
	fixture.revocations.freshness = DefaultAuthorizationFreshness
	fixture.revocations.now = now
	fixture.authenticator.Now = now
	var sessions []*Session
	for i := 0; i < 64; i++ {
		_, state := fixture.addIdentity(fmt.Sprintf("tenant-%02d", i), "skupper-router", "one")
		sessions = append(sessions, fixture.session(state))
	}
	fixture.client.ClearActions()
	for period := 1; period <= 3; period++ {
		advance(time.Minute)
		if got := fixture.auditRound(); got != 64*8 {
			t.Fatalf("period %d observed %d API requests, want %d", period, got, 64*8)
		}
	}
	advance(DefaultAuthorizationFreshness - time.Second)
	if err := sessions[0].Check(); err != nil {
		t.Fatalf("successful audits did not retain authority through the refreshed period: %v", err)
	}
	advance(2 * time.Second)
	if err := sessions[0].Check(); err == nil {
		t.Fatal("session remained authorized beyond the final audit deadline")
	}
	for _, action := range fixture.client.Actions() {
		if action.GetVerb() != "get" {
			t.Fatalf("audit issued %s %s instead of a targeted GET", action.GetVerb(), action.GetResource().Resource)
		}
	}
	t.Log("observed 512 targeted GETs per 64-session audit period; 512/60 = 8.53 GET/s at the production interval")
	for _, session := range sessions {
		session.Close()
	}
}

func TestAuditorDeduplicatesSharedHAIdentityDependencies(t *testing.T) {
	fixture := newAuditorFixture(t)
	_, first := fixture.addIdentity("tenant", "skupper-router", "one")
	_, second := fixture.addIdentity("tenant", "skupper-router-2", "one")
	one, two := fixture.session(first), fixture.session(second)
	defer one.Close()
	defer two.Close()
	now := time.Now()
	fixture.revocations.mu.Lock()
	fixture.revocations.sessions[one].nextAudit = now
	fixture.revocations.sessions[two].nextAudit = now.Add(30 * time.Second)
	fixture.revocations.mu.Unlock()
	fixture.client.ClearActions()
	batches := fixture.revocations.due(now)
	if len(batches) != 1 || len(batches[0].sessions) != 2 {
		t.Fatalf("staggered HA sessions did not join one namespace audit: %#v", batches)
	}
	fixture.revocations.complete(batches[0], fixture.authenticator.audit(context.Background(), batches[0]))
	if got := len(fixture.client.Actions()); got != 11 {
		t.Fatalf("shared HA audit observed %d GETs, want 11 unique dependencies", got)
	}
}

func TestAuditorConcurrencyIsBounded(t *testing.T) {
	fixture := newAuditorFixture(t)
	fixture.revocations.interval = time.Millisecond
	fixture.revocations.tick = time.Millisecond
	fixture.revocations.concurrency = 2
	var active, maximum atomic.Int32
	release := make(chan struct{})
	defer close(release)
	for i := 0; i < 6; i++ {
		_, state := fixture.addIdentity(fmt.Sprintf("tenant-%d", i), "skupper-router", "one")
		fixture.session(state)
	}
	fixture.authenticator.Authorize = func(context.Context, Identity) error {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		<-release
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.authenticator.RunAuditor(ctx) }()
	deadline := time.After(time.Second)
	for maximum.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("auditor did not start bounded workers")
		case <-time.After(time.Millisecond):
		}
	}
	if maximum.Load() > 2 {
		t.Fatalf("audit concurrency = %d, want at most 2", maximum.Load())
	}
	cancel()
	<-done
	fixture.revocations.mu.Lock()
	defer fixture.revocations.mu.Unlock()
	if len(fixture.revocations.inFlight) != 0 {
		t.Fatal("auditor shutdown left namespace batches in flight")
	}
}

func TestPersistentRevocationClosesWithoutWatchDelivery(t *testing.T) {
	fixture := newAuditorFixture(t)
	fixture.revocations.freshness = 80 * time.Millisecond
	fixture.revocations.interval = 20 * time.Millisecond
	fixture.revocations.tick = time.Millisecond
	identity, state := fixture.addIdentity("tenant", "skupper-router", "one")
	session := fixture.session(state)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fixture.authenticator.RunAuditor(ctx)
	if err := fixture.client.CoreV1().Pods(identity.Namespace).Delete(ctx, identity.PodName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Context().Done():
		if err := session.Check(); err == nil {
			t.Fatal("deleted identity closed without an authorization error")
		}
	case <-time.After(80 * time.Millisecond):
		t.Fatal("missing watch delivery allowed authority past its freshness bound")
	}
}

func TestSlowAdmissionCannotOutliveItsOldestRead(t *testing.T) {
	fixture := newAuditorFixture(t)
	fixture.revocations.freshness = 20 * time.Millisecond
	_, state := fixture.addIdentity("tenant", "skupper-router", "one")
	fixture.authenticator.Authorize = func(context.Context, Identity) error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}
	if _, err := fixture.authenticator.Session(context.Background(), state); err == nil || !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("stale admission result = %v, want authorization expiry", err)
	}
}

func TestFailedAndHungAuditsDoNotExtendDeadline(t *testing.T) {
	for _, test := range []struct {
		name      string
		authorize func(context.Context, Identity) error
		release   chan struct{}
	}{
		{name: "transient failure", authorize: func(context.Context, Identity) error { return AuthorizationUnavailable(errors.New("429")) }},
		{name: "hung", release: make(chan struct{})},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAuditorFixture(t)
			fixture.revocations.freshness = 60 * time.Millisecond
			fixture.revocations.retry = 5 * time.Millisecond
			fixture.authenticator.AuthorizationTimeout = 10 * time.Millisecond
			_, state := fixture.addIdentity("tenant", "skupper-router", "one")
			session := fixture.session(state)
			if test.release != nil {
				fixture.authenticator.Authorize = func(context.Context, Identity) error { <-test.release; return nil }
			} else {
				fixture.authenticator.Authorize = test.authorize
			}
			fixture.auditRound()
			select {
			case <-session.Context().Done():
			case <-time.After(200 * time.Millisecond):
				t.Fatal("failed audit extended authorization")
			}
			if test.release != nil {
				close(test.release)
			}
		})
	}
}

func TestTransientAuditsRetryWithinOriginalDeadline(t *testing.T) {
	fixture := newAuditorFixture(t)
	fixture.revocations.freshness = 45 * time.Millisecond
	fixture.revocations.interval = time.Millisecond
	fixture.revocations.retry = 8 * time.Millisecond
	fixture.revocations.tick = time.Millisecond
	_, state := fixture.addIdentity("tenant", "skupper-router", "one")
	session := fixture.session(state)
	var attempts atomic.Int32
	fixture.authenticator.Authorize = func(context.Context, Identity) error {
		attempts.Add(1)
		return AuthorizationUnavailable(errors.New("429"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fixture.authenticator.RunAuditor(ctx)
	select {
	case <-session.Context().Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("transient audits extended the original authorization deadline")
	}
	if got := attempts.Load(); got < 2 || got > 8 {
		t.Fatalf("bounded retry attempts = %d, want 2..8 before expiry", got)
	}
}

func TestInvalidationDuringAuditAndLateSuccessCannotReviveSession(t *testing.T) {
	fixture := newAuditorFixture(t)
	fixture.revocations.freshness = 50 * time.Millisecond
	identity, state := fixture.addIdentity("tenant", "skupper-router", "one")
	session := fixture.session(state)
	now := time.Now()
	fixture.revocations.mu.Lock()
	fixture.revocations.sessions[session].nextAudit = now
	fixture.revocations.mu.Unlock()
	batch := fixture.revocations.due(now)[0]
	started := make(chan struct{})
	release := make(chan struct{})
	fixture.authenticator.Authorize = func(context.Context, Identity) error {
		close(started)
		<-release
		return nil
	}
	results := make(chan map[Identity]error, 1)
	go func() { results <- fixture.authenticator.audit(context.Background(), batch) }()
	<-started
	fixture.revocations.InvalidateAuthorization(AuthorizationPod, identity.Namespace, identity.PodName)
	select {
	case <-session.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("semantic invalidation did not promptly cancel in-flight audit authority")
	}
	close(release)
	fixture.revocations.complete(batch, <-results)
	if err := session.Check(); err == nil {
		t.Fatal("late audit success revived a revoked session")
	}
}

func TestAuditDoesNotRefreshReconnectExcludedFromBatch(t *testing.T) {
	fixture := newAuditorFixture(t)
	fixture.revocations.freshness = 200 * time.Millisecond
	_, state := fixture.addIdentity("tenant", "skupper-router", "one")
	old := fixture.session(state)
	now := time.Now()
	fixture.revocations.mu.Lock()
	fixture.revocations.sessions[old].nextAudit = now
	fixture.revocations.mu.Unlock()
	batch := fixture.revocations.due(now)[0]
	time.Sleep(20 * time.Millisecond)
	replacement := fixture.session(state)
	replacementDeadline := replacement.authorizationExpiry()
	fixture.revocations.complete(batch, map[Identity]error{old.Identity: nil})
	if !replacement.authorizationExpiry().Equal(replacementDeadline) {
		t.Fatal("evidence for the old connection refreshed an unaudited reconnect")
	}
	old.Close()
	replacement.Close()
}

func TestLateAuditSuccessCannotReviveExpiredCertificate(t *testing.T) {
	fixture := newAuditorFixture(t)
	_, state := fixture.addIdentity("tenant", "skupper-router", "one")
	leaf := *state.VerifiedChains[0][0]
	leaf.NotAfter = time.Now().Add(25 * time.Millisecond)
	state.VerifiedChains[0][0] = &leaf
	session := fixture.session(state)
	now := time.Now()
	fixture.revocations.mu.Lock()
	fixture.revocations.sessions[session].nextAudit = now
	fixture.revocations.mu.Unlock()
	batch := fixture.revocations.due(now)[0]
	<-session.Context().Done()
	fixture.revocations.complete(batch, map[Identity]error{session.Identity: nil})
	if err := session.Check(); err == nil {
		t.Fatal("late audit success revived an expired certificate")
	}
}

func TestDegradedAuditRequestBoundUsesTargetedGets(t *testing.T) {
	fixture := newAuditorFixture(t)
	var sessions []*Session
	for i := 0; i < 64; i++ {
		_, state := fixture.addIdentity(fmt.Sprintf("tenant-%02d", i), "skupper-router", "one")
		sessions = append(sessions, fixture.session(state))
	}
	fixture.client.ClearActions()
	fixture.client.PrependReactor("get", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.GetAction).GetName() == "skupper-controller-allocations" {
			return true, nil, errors.New("API unavailable")
		}
		return false, nil, nil
	})
	if got := fixture.auditRound(); got != 64*8 {
		t.Fatalf("degraded audit observed %d requests, want 512 targeted GETs", got)
	}
	t.Log("observed 512 GETs for a failed final dependency; a five-second retry is bounded at 102.4 GET/s until the 120-second freshness deadline closes sessions")
	for _, session := range sessions {
		session.Close()
	}
}
