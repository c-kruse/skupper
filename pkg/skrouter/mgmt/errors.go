package mgmt

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrBadRequest matches a management 400 response.
	ErrBadRequest = statusCodeError(400)
	// ErrForbidden matches a management 403 response.
	ErrForbidden = statusCodeError(403)
	// ErrNotFound matches a management 404 response.
	ErrNotFound = statusCodeError(404)
	// ErrNotImplemented matches a management 501 response.
	ErrNotImplemented = statusCodeError(501)
	// ErrClosed is returned after the client has been closed.
	ErrClosed = errors.New("management client closed")
)

type statusCodeError int

func (e statusCodeError) Error() string {
	return fmt.Sprintf("management status %d", e)
}

// Ref identifies one entity by name or identity.
type Ref struct {
	Name     string
	Identity string
}

// ByName returns an entity reference by name.
func ByName(name string) Ref { return Ref{Name: name} }

// ByIdentity returns an entity reference by identity.
func ByIdentity(identity string) Ref { return Ref{Identity: identity} }

func (r Ref) String() string {
	if r.Identity != "" {
		return "identity=" + r.Identity
	}
	if r.Name != "" {
		return "name=" + r.Name
	}
	return ""
}

func (r Ref) validate() error {
	if (r.Name == "") == (r.Identity == "") {
		return errors.New("an entity reference must set exactly one of name or identity")
	}
	return nil
}

// StatusError is a non-success response from the router management agent.
type StatusError struct {
	Code        int
	Description string
	Operation   string
	Type        string
	Ref         Ref
}

func (e *StatusError) Error() string {
	detail := e.Description
	if detail == "" {
		detail = "management request failed"
	}
	context := strings.TrimSpace(strings.Join([]string{e.Operation, e.Type, e.Ref.String()}, " "))
	if context == "" {
		return fmt.Sprintf("management status %d: %s", e.Code, detail)
	}
	return fmt.Sprintf("management %s: status %d: %s", context, e.Code, detail)
}

// Is supports errors.Is with the exported status sentinels.
func (e *StatusError) Is(target error) bool {
	code, ok := target.(statusCodeError)
	return ok && e.Code == int(code)
}

// TransportError reports a connection, session, or link failure. The request
// may be retried according to the caller's idempotency policy.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string   { return "management transport: " + e.Err.Error() }
func (e *TransportError) Unwrap() error   { return e.Err }
func (e *TransportError) Retryable() bool { return true }

// ValidationError reports fields or an operation rejected before a request is
// sent to the router.
type ValidationError struct {
	Type   string
	Fields []string
	Op     string
}

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return fmt.Sprintf("management type %s does not support %s", e.Type, e.Op)
	}
	return fmt.Sprintf("fields %s are not valid for %s on management type %s", strings.Join(e.Fields, ", "), e.Op, e.Type)
}
