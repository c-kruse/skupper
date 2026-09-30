package routercontrol

import (
	"fmt"
	"sync"
	"time"
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
	namespace uint64
}

// SessionRevocations turns authorization-relevant informer changes into
// immediate cancellation. Revoked active certificate serials remain denied
// until their signed expiry so reconnecting cannot undo watch-driven denial.
// Admission still performs the complete authoritative API validation.
type SessionRevocations struct {
	mu         sync.Mutex
	namespaces map[string]uint64
	sessions   map[*Session]struct{}
	revoked    map[string]time.Time
	now        func() time.Time
}

func NewSessionRevocations() *SessionRevocations {
	return &SessionRevocations{
		namespaces: map[string]uint64{},
		sessions:   map[*Session]struct{}{},
		revoked:    map[string]time.Time{},
		now:        time.Now,
	}
}

func (r *SessionRevocations) begin(namespace string) authorizationRevision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeExpiredLocked(r.now())
	return authorizationRevision{namespace: r.namespaces[namespace]}
}

func (r *SessionRevocations) register(session *Session, revision authorizationRevision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.removeExpiredLocked(now)
	if revision.namespace != r.namespaces[session.Identity.Namespace] {
		return fmt.Errorf("%w: authorization state changed during admission", ErrUnauthorized)
	}
	if expiry, found := r.revoked[session.certificateSerial]; found && now.Before(expiry) {
		return fmt.Errorf("%w: client certificate was revoked", ErrUnauthorized)
	}
	r.sessions[session] = struct{}{}
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
	r.mu.Lock()
	r.namespaces[namespace]++
	r.removeExpiredLocked(r.now())
	var affected []*Session
	for session := range r.sessions {
		if authorizationMatches(session.Identity, kind, namespace, name) {
			r.revoked[session.certificateSerial] = session.certificateExpiry
			affected = append(affected, session)
		}
	}
	r.mu.Unlock()
	for _, session := range affected {
		session.fail(fmt.Errorf("%w: watched %s changed", ErrUnauthorized, kind))
	}
}

func (r *SessionRevocations) removeExpiredLocked(now time.Time) {
	for serial, expiry := range r.revoked {
		if !now.Before(expiry) {
			delete(r.revoked, serial)
		}
	}
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
