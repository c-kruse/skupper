package routercontrol

import (
	"fmt"
	"sync"
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

// SessionRevocations turns shared informer changes into immediate, scoped
// session cancellation. Admission still uses authoritative API reads; the
// revision barrier only ensures no relevant event is lost while those reads
// and session registration race.
type SessionRevocations struct {
	mu         sync.Mutex
	global     uint64
	namespaces map[string]uint64
	sessions   map[*Session]struct{}
}

func NewSessionRevocations() *SessionRevocations {
	return &SessionRevocations{namespaces: map[string]uint64{}, sessions: map[*Session]struct{}{}}
}

func (r *SessionRevocations) begin(namespace string) authorizationRevision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return authorizationRevision{global: r.global, namespace: r.namespaces[namespace]}
}

func (r *SessionRevocations) register(session *Session, revision authorizationRevision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if revision.global != r.global || revision.namespace != r.namespaces[session.Identity.Namespace] {
		return fmt.Errorf("%w: authorization state changed during admission", ErrUnauthorized)
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
	var affected []*Session
	for session := range r.sessions {
		if authorizationMatches(session.Identity, kind, namespace, name) {
			affected = append(affected, session)
		}
	}
	r.mu.Unlock()
	for _, session := range affected {
		session.fail(fmt.Errorf("%w: watched %s changed", ErrUnauthorized, kind))
	}
}

// AuthorizationWatchFailed fails closed for every session admitted before the
// error. Kubernetes reflectors relist before resuming their watches, while any
// later admission still has to pass the complete authoritative API walk. A
// permanently silent failure can therefore never grant indefinite authority:
// the server-observed watch error closes current sessions and the short-lived
// client certificate remains the final bound for a newly admitted session.
func (r *SessionRevocations) AuthorizationWatchFailed(err error) {
	r.mu.Lock()
	r.global++
	var affected []*Session
	for session := range r.sessions {
		affected = append(affected, session)
	}
	r.mu.Unlock()
	for _, session := range affected {
		session.fail(fmt.Errorf("%w: authorization watch failed: %v", ErrUnauthorized, err))
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
		// Unknown authorization inputs fail closed for the namespace.
		return true
	}
}
