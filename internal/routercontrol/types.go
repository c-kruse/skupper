// Package routercontrol defines the shared router intent domain and its control
// protocol. It deliberately does not expose QDR configuration types: an adaptor
// compiles accepted intent into its local router representation.
package routercontrol

import "context"

const (
	ProtocolVersion = "v1"
	SchemaVersion   = "v1"
)

// Digest is a lowercase hexadecimal SHA-256 content identity.
type Digest string

// ResourceID is stable across mutable resource names. Callers should derive it
// from the owning object UID, role, and subtarget.
type ResourceID string

// TargetIdentity identifies a logical router target, not a Pod or process.
type TargetIdentity struct {
	NamespaceUID string `json:"namespaceUid"`
	SiteUID      string `json:"siteUid"`
	RouterGroup  string `json:"routerGroup"`
}

type RoutingMode string

const (
	RoutingModeInterior RoutingMode = "interior"
	RoutingModeEdge     RoutingMode = "edge"
)

type Protocol string

const (
	ProtocolTCP   Protocol = "tcp"
	ProtocolUDP   Protocol = "udp"
	ProtocolHTTP  Protocol = "http"
	ProtocolHTTP2 Protocol = "http2"
)

type TLSMode string

const (
	TLSModeDisabled TLSMode = "disabled"
	TLSModeServer   TLSMode = "server"
	TLSModeClient   TLSMode = "client"
	TLSModeMutual   TLSMode = "mutual"
)

type TLSIntent struct {
	Mode              TLSMode    `json:"mode"`
	CredentialBinding ResourceID `json:"credentialBinding,omitempty"`
	VerifyHostname    bool       `json:"verifyHostname,omitempty"`
}

type RouterSettings struct {
	Mode                RoutingMode        `json:"mode"`
	DataConnectionCount uint32             `json:"dataConnectionCount,omitempty"`
	Logging             []RouterLogSetting `json:"logging,omitempty"`
	OwnedAddressKeys    []string           `json:"ownedAddressKeys,omitempty"`
}

type RouterLogSetting struct {
	// Empty Module denotes the QDR default module.
	Module string `json:"module,omitempty"`
	Level  string `json:"level"`
}

type RouterConnection struct {
	ID                     ResourceID `json:"id"`
	Host                   string     `json:"host"`
	Port                   uint16     `json:"port"`
	Role                   string     `json:"role"`
	Cost                   uint32     `json:"cost,omitempty"`
	TLS                    TLSIntent  `json:"tls"`
	ProxyCredentialBinding ResourceID `json:"proxyCredentialBinding,omitempty"`
}

type RouterListener struct {
	ID   ResourceID `json:"id"`
	Host string     `json:"host"`
	Port uint16     `json:"port"`
	Role string     `json:"role"`
	TLS  TLSIntent  `json:"tls"`
}

// RoutingKeys is ordered by routing priority and is never sorted during
// normalization.
type ServiceListener struct {
	ID          ResourceID `json:"id"`
	Host        string     `json:"host"`
	Port        uint16     `json:"port"`
	Protocol    Protocol   `json:"protocol"`
	RoutingKeys []string   `json:"routingKeys"`
	Observer    string     `json:"observer,omitempty"`
	TLS         TLSIntent  `json:"tls"`
}

type Endpoint struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Port uint16 `json:"port"`
}

type ServiceConnector struct {
	ID         ResourceID     `json:"id"`
	RoutingKey string         `json:"routingKey"`
	Protocol   Protocol       `json:"protocol"`
	Endpoints  []Endpoint     `json:"endpoints"`
	Target     TargetIdentity `json:"target"`
	TLS        TLSIntent      `json:"tls"`
}

// CredentialBinding contains only a provider reference and requirements. It
// must never contain credential bytes, private keys, file paths, or local QDR
// profile ordinals.
type CredentialBinding struct {
	ID         ResourceID           `json:"id"`
	Provider   string               `json:"provider"`
	Reference  string               `json:"reference"`
	Usages     []string             `json:"usages"`
	Properties []CredentialProperty `json:"properties,omitempty"`
}

type CredentialProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// RouterIntent is the complete controller-owned domain document for Target.
// Empty resource slices explicitly mean that no traffic resources are desired.
type RouterIntent struct {
	SchemaVersion      string              `json:"schemaVersion"`
	Target             TargetIdentity      `json:"target"`
	Settings           RouterSettings      `json:"settings"`
	RouterConnections  []RouterConnection  `json:"routerConnections"`
	RouterListeners    []RouterListener    `json:"routerListeners"`
	ServiceListeners   []ServiceListener   `json:"serviceListeners"`
	ServiceConnectors  []ServiceConnector  `json:"serviceConnectors"`
	CredentialBindings []CredentialBinding `json:"credentialBindings"`
}

// IntentPublisher is the namespace reconciler's non-blocking publication hook.
// Publish validates and snapshots intent. SetUnavailable means no derived result
// exists; it never means an empty intent and never requests router deletion.
type IntentPublisher interface {
	Publish(intent RouterIntent) (Digest, error)
	SetUnavailable(target TargetIdentity)
}

// SessionIdentity is established by authentication, never by Hello claims. A
// Pod rollout may have multiple identities concurrently realizing one target.
type SessionIdentity struct {
	PodUID            string
	ServiceAccountUID string
}

// SessionKey identifies one authenticated Pod realization of a target.
type SessionKey struct {
	Target   TargetIdentity
	Identity SessionIdentity
}

// AuthorizeFunc is supplied by the authentication layer. The context contains
// the TLS-authenticated peer identity; successful return authorizes exactly the
// target from Hello and returns the authenticated Pod identity. Router control
// never installs an auth bypass.
type AuthorizeFunc func(ctx context.Context, target TargetIdentity) (SessionIdentity, error)

type Hello struct {
	ProtocolVersions  []string       `json:"protocolVersions"`
	SchemaVersions    []string       `json:"schemaVersions"`
	Capabilities      []string       `json:"capabilities,omitempty"`
	AdaptorInstanceID string         `json:"adaptorInstanceId"`
	RouterIncarnation string         `json:"routerIncarnation"`
	Target            TargetIdentity `json:"target"`
}

type Welcome struct {
	ProtocolVersion string         `json:"protocolVersion"`
	SchemaVersion   string         `json:"schemaVersion"`
	SessionID       string         `json:"sessionId"`
	Target          TargetIdentity `json:"target"`
}

type TransferKind string

const (
	TransferSnapshot TransferKind = "snapshot"
	TransferDelta    TransferKind = "delta"
)

type TransferBegin struct {
	SessionID     string       `json:"sessionId"`
	TransactionID string       `json:"transactionId"`
	Sequence      uint64       `json:"sequence"`
	Kind          TransferKind `json:"kind"`
	BaseDigest    Digest       `json:"baseDigest,omitempty"`
	ResultDigest  Digest       `json:"resultDigest"`
	EncodedSize   uint64       `json:"encodedSize"`
	ChunkCount    uint32       `json:"chunkCount"`
}

type TransferChunk struct {
	SessionID     string `json:"sessionId"`
	TransactionID string `json:"transactionId"`
	Index         uint32 `json:"index"`
	Data          []byte `json:"data"`
}

type TransferEnd struct {
	SessionID     string `json:"sessionId"`
	TransactionID string `json:"transactionId"`
	Complete      bool   `json:"complete"`
}

type ResourceRef struct {
	Kind string     `json:"kind"`
	ID   ResourceID `json:"id"`
}

// IntentDelta carries typed upserts and explicit deletions. It is applied to a
// copy of the accepted base and validated as a complete RouterIntent before swap.
type IntentDelta struct {
	SchemaVersion      string              `json:"schemaVersion"`
	Target             TargetIdentity      `json:"target"`
	BaseDigest         Digest              `json:"baseDigest"`
	ResultDigest       Digest              `json:"resultDigest"`
	Settings           *RouterSettings     `json:"settings,omitempty"`
	RouterConnections  []RouterConnection  `json:"routerConnections,omitempty"`
	RouterListeners    []RouterListener    `json:"routerListeners,omitempty"`
	ServiceListeners   []ServiceListener   `json:"serviceListeners,omitempty"`
	ServiceConnectors  []ServiceConnector  `json:"serviceConnectors,omitempty"`
	CredentialBindings []CredentialBinding `json:"credentialBindings,omitempty"`
	Deletes            []ResourceRef       `json:"deletes,omitempty"`
}

type Accepted struct {
	SessionID     string `json:"sessionId"`
	TransactionID string `json:"transactionId"`
	Sequence      uint64 `json:"sequence"`
	Digest        Digest `json:"digest"`
}

type Rejected struct {
	SessionID     string `json:"sessionId"`
	TransactionID string `json:"transactionId,omitempty"`
	Sequence      uint64 `json:"sequence,omitempty"`
	Reason        string `json:"reason"`
}

type ResyncRequested struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason"`
}

type ApplicationState string

const (
	ApplicationPending ApplicationState = "pending"
	ApplicationApplied ApplicationState = "applied"
	ApplicationFailed  ApplicationState = "failed"
)

type ResourceApplication struct {
	ResourceID    ResourceID       `json:"resourceId"`
	RealizationID string           `json:"realizationId,omitempty"`
	State         ApplicationState `json:"state"`
	Reason        string           `json:"reason,omitempty"`
}

type ApplicationReport struct {
	SessionID         string                `json:"sessionId"`
	Sequence          uint64                `json:"sequence"`
	IntentDigest      Digest                `json:"intentDigest"`
	RouterIncarnation string                `json:"routerIncarnation"`
	RealizationID     string                `json:"realizationId"`
	Credentials       []CredentialRevision  `json:"credentials,omitempty"`
	State             ApplicationState      `json:"state"`
	Resources         []ResourceApplication `json:"resources,omitempty"`
}

// CredentialRevision identifies the non-secret provider material used by one
// realization. Rotation changes Revision and therefore requires new Applied
// evidence even when the intent digest is unchanged.
type CredentialRevision struct {
	BindingID ResourceID `json:"bindingId"`
	Revision  string     `json:"revision"`
}

type Knowledge string

const (
	KnowledgeComplete Knowledge = "complete"
	KnowledgePartial  Knowledge = "partial"
	KnowledgeUnknown  Knowledge = "unknown"
)

const (
	ObservationScopeResources = "resources"
	ObservationScopeAddresses = "addresses"
)

type OperationalState string

const (
	OperationalUnknown OperationalState = "unknown"
	OperationalUp      OperationalState = "up"
	OperationalDown    OperationalState = "down"
)

type LocalResourceObservation struct {
	ResourceID    ResourceID       `json:"resourceId"`
	RealizationID string           `json:"realizationId,omitempty"`
	Operational   OperationalState `json:"operational"`
	Message       string           `json:"message,omitempty"`
}

// LocalAddressObservation reports the local router's exact routing-address
// facts independently of listener socket state. Counts are never inferred from
// absent records; they are meaningful only in a successful complete/partial
// addresses scope.
type LocalAddressObservation struct {
	RoutingKey      string `json:"routingKey"`
	Reachable       bool   `json:"reachable"`
	SubscriberCount uint64 `json:"subscriberCount"`
	InProcessCount  uint64 `json:"inProcessCount"`
	RemoteCount     uint64 `json:"remoteCount"`
}

// ObservationSnapshot is complete, partial, or unknown for one named local
// scope. Complete with zero Resources is known empty. Unknown is not empty.
type ObservationSnapshot struct {
	SessionID         string                     `json:"sessionId"`
	Scope             string                     `json:"scope"`
	SampleSequence    uint64                     `json:"sampleSequence"`
	RefreshRequestID  string                     `json:"refreshRequestId,omitempty"`
	Knowledge         Knowledge                  `json:"knowledge"`
	RouterIncarnation string                     `json:"routerIncarnation"`
	Resources         []LocalResourceObservation `json:"resources,omitempty"`
	Addresses         []LocalAddressObservation  `json:"addresses,omitempty"`
	Reason            string                     `json:"reason,omitempty"`
}

type Heartbeat struct {
	SessionID string `json:"sessionId"`
}

type RefreshRequest struct {
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Scope     string `json:"scope"`
}

// ClientMessage and ServerMessage are explicit protocol unions. Validation
// requires exactly one field to be set.
type ClientMessage struct {
	Hello             *Hello               `json:"hello,omitempty"`
	Accepted          *Accepted            `json:"accepted,omitempty"`
	Rejected          *Rejected            `json:"rejected,omitempty"`
	ResyncRequested   *ResyncRequested     `json:"resyncRequested,omitempty"`
	ApplicationReport *ApplicationReport   `json:"applicationReport,omitempty"`
	Observation       *ObservationSnapshot `json:"observation,omitempty"`
	Heartbeat         *Heartbeat           `json:"heartbeat,omitempty"`
}

type ServerMessage struct {
	Welcome           *Welcome        `json:"welcome,omitempty"`
	IntentUnavailable bool            `json:"intentUnavailable,omitempty"`
	Begin             *TransferBegin  `json:"begin,omitempty"`
	Chunk             *TransferChunk  `json:"chunk,omitempty"`
	End               *TransferEnd    `json:"end,omitempty"`
	RefreshRequest    *RefreshRequest `json:"refreshRequest,omitempty"`
}

// ObservationSink receives already session-bound reports. Implementations
// should copy retained values and must not block the stream indefinitely.
type ObservationSink interface {
	Application(ctx context.Context, key SessionKey, report ApplicationReport) error
	Observation(ctx context.Context, key SessionKey, observation ObservationSnapshot) error
	Disconnected(key SessionKey, sessionID string)
}
