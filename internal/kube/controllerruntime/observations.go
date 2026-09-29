package controllerruntime

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skupperproject/skupper/internal/kube/reconcile"
	protocol "github.com/skupperproject/skupper/internal/routercontrol"
)

const (
	observationRefreshEvery = 10 * time.Second
	observationDeadline     = 10 * time.Second
	observationFreshFor     = 30 * time.Second
)

type refreshSender interface {
	RequestRefresh(protocol.SessionKey, string, string, string) error
}

type pendingRefresh struct {
	id       string
	sent     time.Time
	deadline time.Time
}

type cachedScope struct {
	snapshot       protocol.ObservationSnapshot
	sequence       uint64
	freshUntil     time.Time
	expiryNotified bool
	nextRefresh    time.Time
	pending        *pendingRefresh
}

type cachedSession struct {
	id          string
	incarnation string
	accepted    protocol.Accepted
	application *protocol.ApplicationReport
	scopes      map[string]*cachedScope
}

// observationCache holds authenticated, per-Pod facts, not desired state.
// Only a timely reply to a controller-issued refresh can extend freshness.
// Heartbeats and buffered unsolicited samples cannot keep old facts fresh.
type observationCache struct {
	mu         sync.Mutex
	namespaces map[string]string
	sessions   map[protocol.SessionKey]*cachedSession
	invalidate func(...string)
	now        func() time.Time
}

func newObservationCache(invalidate func(...string)) *observationCache {
	return &observationCache{
		namespaces: map[string]string{},
		sessions:   map[protocol.SessionKey]*cachedSession{},
		invalidate: invalidate,
		now:        time.Now,
	}
}

// NoteNamespace receives the name from live TLS authorization, never from Hello.
func (c *observationCache) NoteNamespace(uid, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.namespaces[uid] = name
}

func (c *observationCache) Connected(key protocol.SessionKey, sessionID string, hello protocol.Hello) {
	c.mu.Lock()
	c.sessions[key] = &cachedSession{
		id: sessionID, incarnation: hello.RouterIncarnation,
		scopes: map[string]*cachedScope{
			protocol.ObservationScopeResources: {},
			protocol.ObservationScopeAddresses: {},
		},
	}
	namespace := c.namespaces[key.Target.NamespaceUID]
	c.mu.Unlock()
	c.changed(namespace)
}

func (c *observationCache) Accepted(key protocol.SessionKey, accepted protocol.Accepted) {
	c.mu.Lock()
	session := c.sessions[key]
	if session == nil || session.id != accepted.SessionID || accepted.Sequence <= session.accepted.Sequence {
		c.mu.Unlock()
		return
	}
	session.accepted = accepted
	session.application = nil
	session.invalidateScopes()
	namespace := c.namespaces[key.Target.NamespaceUID]
	c.mu.Unlock()
	c.changed(namespace)
}

func (c *observationCache) Application(ctx context.Context, key protocol.SessionKey, report protocol.ApplicationReport) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	session := c.sessions[key]
	if session == nil || session.id != report.SessionID || session.incarnation != report.RouterIncarnation {
		c.mu.Unlock()
		return fmt.Errorf("application report does not belong to the current router session")
	}
	if report.Sequence != session.accepted.Sequence || report.IntentDigest != session.accepted.Digest {
		c.mu.Unlock()
		return nil // An older, once-accepted application cannot satisfy newer intent.
	}
	next := copyApplication(&report)
	if reflect.DeepEqual(session.application, next) {
		c.mu.Unlock()
		return nil // Periodic verified readback has not changed the evidence.
	}
	if session.application == nil || session.application.RealizationID != report.RealizationID || session.application.State != report.State {
		session.invalidateScopes()
	}
	session.application = next
	namespace := c.namespaces[key.Target.NamespaceUID]
	c.mu.Unlock()
	c.changed(namespace)
	return nil
}

// An old refresh cannot certify facts for newly accepted intent or a new
// credential realization, even when the transport session has not changed.
func (s *cachedSession) invalidateScopes() {
	for _, scope := range s.scopes {
		sequence := scope.sequence
		*scope = cachedScope{sequence: sequence}
	}
}

func (c *observationCache) Observation(ctx context.Context, key protocol.SessionKey, observation protocol.ObservationSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := protocol.ValidateObservation(observation); err != nil {
		return err
	}
	c.mu.Lock()
	session := c.sessions[key]
	if session == nil || session.id != observation.SessionID || session.incarnation != observation.RouterIncarnation {
		c.mu.Unlock()
		return fmt.Errorf("observation does not belong to the current router session")
	}
	scope := session.scopes[observation.Scope]
	if observation.SampleSequence <= scope.sequence {
		c.mu.Unlock()
		return nil
	}
	now := c.now()
	wasFresh := now.Before(scope.freshUntil)
	// A late response is not allowed to overwrite newer facts or authenticate
	// their freshness. A new session starts with neither a sample nor freshness.
	if observation.RefreshRequestID != "" {
		pending := scope.pending
		if pending == nil || pending.id != observation.RefreshRequestID || !now.Before(pending.deadline) {
			c.mu.Unlock()
			return nil
		}
		scope.freshUntil = pending.sent.Add(observationFreshFor)
		scope.expiryNotified = false
		scope.pending = nil
	}
	if observation.Knowledge == protocol.KnowledgeUnknown {
		scope.freshUntil = time.Time{}
	}
	// Renew freshness and retain sequence fencing even for identical facts, but
	// do not turn each periodic refresh into a complete namespace effect plan.
	changed := wasFresh != now.Before(scope.freshUntil) ||
		scope.snapshot.Knowledge != observation.Knowledge || scope.snapshot.Reason != observation.Reason ||
		!slices.Equal(scope.snapshot.Resources, observation.Resources) || !slices.Equal(scope.snapshot.Addresses, observation.Addresses)
	scope.sequence = observation.SampleSequence
	scope.snapshot = copyObservation(observation)
	namespace := c.namespaces[key.Target.NamespaceUID]
	c.mu.Unlock()
	if changed {
		c.changed(namespace)
	}
	return nil
}

func (c *observationCache) Disconnected(key protocol.SessionKey, sessionID string) {
	c.mu.Lock()
	session := c.sessions[key]
	if session == nil || session.id != sessionID {
		c.mu.Unlock()
		return
	}
	delete(c.sessions, key)
	namespace := c.namespaces[key.Target.NamespaceUID]
	c.mu.Unlock()
	c.changed(namespace)
}

func (c *observationCache) Snapshot(namespace string, evaluationTime time.Time) map[protocol.TargetIdentity][]reconcile.Observation {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := map[protocol.TargetIdentity][]reconcile.Observation{}
	for key, session := range c.sessions {
		if c.namespaces[key.Target.NamespaceUID] != namespace {
			continue
		}
		observation := reconcile.Observation{
			Key: key, SessionID: session.id, AcceptedDigest: session.accepted.Digest,
			Application: copyApplication(session.application), Scopes: map[string]reconcile.ObservationScope{},
		}
		for name, scope := range session.scopes {
			observation.Scopes[name] = reconcile.ObservationScope{
				Fresh: evaluationTime.Before(scope.freshUntil), Snapshot: copyObservation(scope.snapshot),
			}
		}
		result[key.Target] = append(result[key.Target], observation)
	}
	return result
}

func (c *observationCache) Run(ctx context.Context, sender refreshSender) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(sender)
		}
	}
}

func (c *observationCache) refresh(sender refreshSender) {
	type request struct {
		key       protocol.SessionKey
		sessionID string
		scope     string
		id        string
	}
	var requests []request
	changed := map[string]bool{}
	c.mu.Lock()
	now := c.now()
	for key, session := range c.sessions {
		for name, scope := range session.scopes {
			if !scope.freshUntil.IsZero() && !now.Before(scope.freshUntil) && !scope.expiryNotified {
				scope.expiryNotified = true
				changed[c.namespaces[key.Target.NamespaceUID]] = true
			}
			if scope.pending != nil && !now.Before(scope.pending.deadline) {
				scope.pending = nil
			}
			if scope.pending != nil || now.Before(scope.nextRefresh) {
				continue
			}
			id := uuid.NewString()
			scope.pending = &pendingRefresh{id: id, sent: now, deadline: now.Add(observationDeadline)}
			scope.nextRefresh = now.Add(observationRefreshEvery)
			requests = append(requests, request{key: key, sessionID: session.id, scope: name, id: id})
		}
	}
	c.mu.Unlock()
	for namespace := range changed {
		c.changed(namespace)
	}
	for _, request := range requests {
		if err := sender.RequestRefresh(request.key, request.sessionID, request.id, request.scope); err != nil {
			c.mu.Lock()
			if session := c.sessions[request.key]; session != nil && session.id == request.sessionID {
				scope := session.scopes[request.scope]
				if scope.pending != nil && scope.pending.id == request.id {
					scope.pending = nil
					scope.nextRefresh = now.Add(time.Second)
				}
			}
			c.mu.Unlock()
		}
	}
}

func (c *observationCache) changed(namespace string) {
	if namespace != "" && c.invalidate != nil {
		c.invalidate(namespace)
	}
}

func copyApplication(report *protocol.ApplicationReport) *protocol.ApplicationReport {
	if report == nil {
		return nil
	}
	copy := *report
	copy.Resources = append([]protocol.ResourceApplication(nil), report.Resources...)
	copy.Credentials = append([]protocol.CredentialRevision(nil), report.Credentials...)
	return &copy
}

func copyObservation(observation protocol.ObservationSnapshot) protocol.ObservationSnapshot {
	observation.Resources = append([]protocol.LocalResourceObservation(nil), observation.Resources...)
	observation.Addresses = append([]protocol.LocalAddressObservation(nil), observation.Addresses...)
	return observation
}
