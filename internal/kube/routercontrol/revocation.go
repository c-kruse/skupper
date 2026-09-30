package routercontrol

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

const (
	DefaultAuthorizationFreshness = 2 * time.Minute
	defaultAuditInterval          = time.Minute
	defaultAuditRetry             = 5 * time.Second
	defaultAuditTick              = time.Second
	defaultAuditConcurrency       = 4
)

type AuthorizationKind string

const (
	AuthorizationNamespace      AuthorizationKind = "namespace"
	AuthorizationAssignment     AuthorizationKind = "assignment"
	AuthorizationAllocation     AuthorizationKind = "allocation"
	AuthorizationPod            AuthorizationKind = "pod"
	AuthorizationServiceAccount AuthorizationKind = "service-account"
	AuthorizationSite           AuthorizationKind = "site"
	AuthorizationRouterGroup    AuthorizationKind = "router-group"
)

type authorizationRevision struct {
	global    uint64
	namespace uint64
}

type auditedSession struct {
	nextAudit time.Time
}

type authorizationAuditBatch struct {
	namespace     string
	revision      authorizationRevision
	evidenceStart time.Time
	sessions      map[Identity][]*Session
}

// SessionRevocations combines prompt informer-driven cancellation with shared
// authoritative audits. Admission and every successful audit establish a hard
// authorization deadline; watches reduce revocation latency but are not trusted
// as the only freshness mechanism.
type SessionRevocations struct {
	mu             sync.Mutex
	global         uint64
	namespaces     map[string]uint64
	sessions       map[*Session]*auditedSession
	inFlight       map[string]bool
	auditNotBefore map[string]time.Time
	early          chan struct{}
	now            func() time.Time
	freshness      time.Duration
	interval       time.Duration
	retry          time.Duration
	tick           time.Duration
	concurrency    int
}

func NewSessionRevocations() *SessionRevocations {
	return &SessionRevocations{
		namespaces: map[string]uint64{}, sessions: map[*Session]*auditedSession{}, inFlight: map[string]bool{}, auditNotBefore: map[string]time.Time{}, early: make(chan struct{}, 1),
		now: time.Now, freshness: DefaultAuthorizationFreshness, interval: defaultAuditInterval, retry: defaultAuditRetry, tick: defaultAuditTick, concurrency: defaultAuditConcurrency,
	}
}

func (r *SessionRevocations) begin(namespace string) authorizationRevision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return authorizationRevision{global: r.global, namespace: r.namespaces[namespace]}
}

func (r *SessionRevocations) register(session *Session, revision authorizationRevision, evidenceStart time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if revision.global != r.global || revision.namespace != r.namespaces[session.Identity.Namespace] {
		return fmt.Errorf("%w: authorization state changed during admission", ErrUnauthorized)
	}
	r.sessions[session] = &auditedSession{nextAudit: evidenceStart.Add(auditSpread(session.Identity.Namespace, r.interval))}
	return nil
}

func (r *SessionRevocations) unregister(session *Session) {
	r.mu.Lock()
	delete(r.sessions, session)
	r.mu.Unlock()
}

// InvalidateAuthorization implements controller.AuthorizationInvalidator.
// Names are interpreted according to kind and only matching sessions close.
func (r *SessionRevocations) InvalidateAuthorization(kind AuthorizationKind, namespace, name string) {
	now := r.now()
	r.mu.Lock()
	r.namespaces[namespace]++
	var affected []*Session
	for session, state := range r.sessions {
		if session.Identity.Namespace == namespace {
			state.nextAudit = now
		}
		if authorizationMatches(session.Identity, kind, namespace, name) {
			affected = append(affected, session)
		}
	}
	r.mu.Unlock()
	for _, session := range affected {
		session.fail(fmt.Errorf("%w: watched %s changed", ErrUnauthorized, kind))
	}
	r.requestEarlyAudit()
}

// AuthorizationWatchFailed makes in-progress evidence stale and requests one
// coalesced early audit. It is diagnostic acceleration, not freshness proof:
// audits and per-session deadlines remain authoritative if no callback arrives.
func (r *SessionRevocations) AuthorizationWatchFailed(error) {
	now := r.now()
	r.mu.Lock()
	r.global++
	for _, state := range r.sessions {
		state.nextAudit = now
	}
	r.mu.Unlock()
	r.requestEarlyAudit()
}

func (r *SessionRevocations) requestEarlyAudit() {
	select {
	case r.early <- struct{}{}:
	default:
	}
}

func (r *SessionRevocations) due(now time.Time) []authorizationAuditBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	byNamespace := map[string]*authorizationAuditBatch{}
	for session, state := range r.sessions {
		if r.inFlight[session.Identity.Namespace] || r.auditNotBefore[session.Identity.Namespace].After(now) || state.nextAudit.After(now) || session.Context().Err() != nil || !now.Before(session.authorizationExpiry()) {
			continue
		}
		if byNamespace[session.Identity.Namespace] == nil {
			byNamespace[session.Identity.Namespace] = &authorizationAuditBatch{namespace: session.Identity.Namespace, revision: authorizationRevision{global: r.global, namespace: r.namespaces[session.Identity.Namespace]}, evidenceStart: now, sessions: map[Identity][]*Session{}}
		}
	}
	// Audit a due namespace together even if its HA sessions were admitted at
	// different times, so shared reads and subsequent schedules stay shared.
	for session := range r.sessions {
		if batch := byNamespace[session.Identity.Namespace]; batch != nil && session.Context().Err() == nil && now.Before(session.authorizationExpiry()) {
			batch.sessions[session.Identity] = append(batch.sessions[session.Identity], session)
		}
	}
	result := make([]authorizationAuditBatch, 0, len(byNamespace))
	for namespace, batch := range byNamespace {
		r.inFlight[namespace] = true
		result = append(result, *batch)
	}
	return result
}

func (r *SessionRevocations) complete(batch authorizationAuditBatch, results map[Identity]error) {
	now := r.now()
	r.mu.Lock()
	delete(r.inFlight, batch.namespace)
	// Repeated watch errors must not bypass the audit retry limit.
	r.auditNotBefore[batch.namespace] = now.Add(r.retry)
	revisionCurrent := batch.revision.global == r.global && batch.revision.namespace == r.namespaces[batch.namespace]
	var reject []*Session
	for identity, included := range batch.sessions {
		err, audited := results[identity]
		for _, session := range included {
			state, active := r.sessions[session]
			if !active {
				continue
			}
			switch {
			case audited && err == nil && revisionCurrent:
				state.nextAudit = batch.evidenceStart.Add(r.interval)
				session.renewAuthorization(batch.evidenceStart, r.freshness)
			case audited && authorizationDefinitive(err):
				reject = append(reject, session)
			default:
				state.nextAudit = now.Add(r.retry)
			}
		}
	}
	r.mu.Unlock()
	for _, session := range reject {
		session.fail(fmt.Errorf("%w: authoritative audit rejected identity", ErrUnauthorized))
	}
}

func (r *SessionRevocations) freshnessDeadline(evidenceStart, certificateExpiry time.Time) time.Time {
	deadline := evidenceStart.Add(r.freshness)
	if certificateExpiry.Before(deadline) {
		return certificateExpiry
	}
	return deadline
}

func auditSpread(namespace string, interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(namespace))
	return time.Duration(hash.Sum64() % uint64(interval))
}

func authorizationMatches(identity Identity, kind AuthorizationKind, namespace, name string) bool {
	if identity.Namespace != namespace {
		return false
	}
	switch kind {
	case AuthorizationNamespace, AuthorizationAssignment, AuthorizationAllocation:
		return true
	case AuthorizationPod:
		return identity.PodName == name
	case AuthorizationServiceAccount:
		return identity.ServiceAccount == name
	case AuthorizationSite:
		return identity.SiteName == name
	case AuthorizationRouterGroup:
		return identity.Group == name
	default:
		return true
	}
}

func (a *Authenticator) RunAuditor(ctx context.Context) error {
	if a.Revocations == nil {
		return fmt.Errorf("router-control authorization revocations are not configured")
	}
	r := a.Revocations
	concurrency := r.concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	semaphore := make(chan struct{}, concurrency)
	var workers sync.WaitGroup
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	defer workers.Wait()
	launch := func() {
		for _, batch := range r.due(r.now()) {
			batch := batch
			workers.Add(1)
			go func() {
				defer workers.Done()
				select {
				case semaphore <- struct{}{}:
					defer func() { <-semaphore }()
				case <-ctx.Done():
					r.complete(batch, nil)
					return
				}
				r.complete(batch, a.audit(ctx, batch))
			}()
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			launch()
		case <-r.early:
			launch()
		}
	}
}

func (a *Authenticator) audit(ctx context.Context, batch authorizationAuditBatch) map[Identity]error {
	results := map[Identity]error{}
	batchContext := withAuthorizationReadCache(ctx)
	for identity, sessions := range batch.sessions {
		if ctx.Err() != nil {
			break
		}
		for _, session := range sessions {
			if session.Check() == nil {
				results[identity] = a.authorizeWithin(batchContext, identity)
				break
			}
		}
	}
	return results
}
