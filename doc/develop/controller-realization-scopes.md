# Controller realization scopes

A realization scope is a grouping of output resources and fields owned by the
skupper controller and the input resources that must be considered together in
order to produce those outputs. The principal scope, the Site scope, encloses
everything pertaining to the operation of an individual Site namespace: the
router deployment and configuration, endpoint types, listener services, and all
inputs required to produce it.

**Status: In Progress.** This document describes realization scopes as intended. An incremental
migration is tracked in #2616.

## The Skupper Controller High Level Description

The skupper controller uses familiar kubernetes controller types and patterns
in order to do work. It DOES NOT use the popular controller runtime packages,
but instead uses the primitives in client-go.

When the controller starts, it registers a collection of Informers that
register watchers with the kubernetes API for collections of resources,
synchronizes a local cache of those resources, keeps them in sync and, fires
callbacks for change events. When the controller gets a callback for one of
these resource events it enqueues a work item (key, handler) in a deduplicating
work queue. A single worker runs on a loop pulling and handling work items
sequentially.

### Resource Scope Routing

Resource event handlers perform minimal work. They evaluate the resource that
changed to determine which scope(s) it may effect. For example, a Link in
namespace "east" concerns the Site scope for namespace "east". Relevant scopes
are then queued with their handler, and the resource handler returns.

Static registration and Certificate/SecuredAccess dependency routing live in
`internal/kube/controller/routing.go`, entered through `registerRouting`.
Informer intake queues a resource handler and namespace/name key; that handler
reads the **current cache**, including nil after deletion. It acknowledges
Certificate synchronous authoring observations before enqueueing stable scope
handlers on the same serial EventProcessor. Domain packages own cache snapshots,
derivation, plans, execution, authorization and synchronous intent authoring;
they do not register these scopes' watches or route invalidations.

This is an incremental boundary, not a Site/Grant migration. Legacy Site resource
callbacks (including Access endpoint propagation to RouterAccess), filtered
Service watches, recovery's second Pod sync, remembered mappings and dynamic
subscriptions retain their existing behavior. Grants still register their
internal watches through one explicit call here. Only the equivalent broad
Certificate inventories are shared; optional providers are registered at setup
and require restart after installation. An unavailable cache is not an empty
synced inventory. Existing shutdown/dynamic subscription lifetimes are unchanged.

Access routing explicitly includes the notified key when its definition is
absent and discovers old Certificate targets and missing owners from unfiltered
surviving outputs. After five queue requeues, recovery needs another notification,
a surviving input/output resync, or restart inventory; deleted definitions do
not resync. Only startup and the named controller-namespace `skupper` Gateway
watch wake the singleton Gateway writer. Equal-resourceVersion notifications
remain enabled for time-based renewal and eventual policy/control propagation;
this consolidation does not add immediate policy/control fanout.

### Scope Handlers

Scope handlers follow a structure meant to reduce potential for side effects,
aide in testability, and minimize API resource utilization.

* **Snapshot** the synchronized informer caches and copy all observed inputs
  and outputs.
* **Derive** the desired state of output resources and fields from the frozen
  snapshot.
* **Plan** a minimal set of mutations to realize the desired state.
* **Execute** performs API operations, credential generation, runtime
  configuration changes, etc.
* **Log** actions attempted and their outcomes.

### Outputs written with server-side apply

Build a desired apply configuration containing only what this scope authors,
not a copy of the observed object. Authorize the resource independently of SSA
field ownership. Gateway and TLSRoute scopes always plan an apply of that
configuration, delegating merge, defaulting and ownership conflict detection to
the API server instead of predicting them locally. Other outputs retain their
minimal-update plans.

Successful applies are recorded against the observed object's UID and
resourceVersion, authored intent and field manager. A matching receipt skips the
request, so settled resyncs issue no PATCHes while those receipts remain cached.
Restart or eviction requires a fresh server evaluation; a changed observation or
intent also requires another apply. Receipts do not predict what admission would
do if rerun, and have no automatic admission-policy invalidation.

For requests that are sent, retain the observed resourceVersion as an update
precondition and use resourceVersion `0` for an absent/create plan. A successful
update whose returned resourceVersion equals that precondition is unchanged:
log it at DEBUG and count the attempted action in the summary. A different
returned version is a stored change, possibly ownership metadata or persisted
defaults rather than a spec repair; log the apply at INFO. Never classify a
failure/timeout as unchanged, or remove preconditions/force ownership to hide a
conflict. Errors still reach the retrying event processor even when status
reporting succeeds.

Metadata policy evaluation reads full observed labels so selectors and lexical
template chaining work. Its outputs, built-ins, and previously applied metadata
values retained after template removal enter the desired configuration. Merely
observed metadata and external owner references do not. Empty authored metadata
maps are omitted, not sent as `{}`: SSA can otherwise acquire map-level ownership
and interfere with independent writers. `resource.AppliedFields` reads exact-version
Apply ownership for metadata retention, not resource authorization or drift
comparison. Previously captured
foreign keys cannot be distinguished retrospectively from authored keys using
managedFields alone; this is not an automatic cleanup of already polluted ownership.

## Site Scope

**Key:** namespace. Only one active Site per namespace.

**Inputs:** All Site resources in the namespace, Listener, MultiKeyListener,
Connector, Link, RouterAccess, AttachedConnectorBindings and eligible
AttachedConnectors, Connector targeted selector Pods, router Pods, the
skupper-network-status ConfigMap, TLS and proxy Secrets, router configuration
ConfigMaps (for port allocation), router Deployments, Services and RBAC, Certificate
and SecuredAccess, sizing, metadata, AccessTokens.

**Outputs:** router Deployments with Pod template, router configuration
ConfigMaps, generated ServiceAccount, Role and RoleBindings when Site does not
specify a ServiceAccount. Local router and listener Services. Site
Certificates. RouterAccess when Site is configured with linkAccess.Site,
SecuredAccess, endpoint/Link/RouterAccess status. Annotations of certain
tlsCredentials Secrets.

**Preserves** existing active Site, service port mappings, ssl profile ordinals
and oldest-valid ordinal.

## Certificate Scope

**Key:** Certificate namespace + name. Certificates are evaluated on their own.

**Inputs:** Certificate spec, owner references and annotations. The same-name
Secret resource. CA Secret. The current time.

**Outputs:** The same-name Secret data and metadata. Certificate status.

**Preserves** Certificate key data when still valid.

The Certificate scope realizes Certificate resources; it does not author their
specs or aggregate SecuredAccess hostname requests.

## SecuredAccess Scope

**Key:** SecuredAccess namespace + name. SecuredAccesses are evaluated on their own.

**Inputs:** SecuredAccess spec and owner references. Same-name Service and
provider resources (Route, Ingress, HTTPProxy, TLSRoute.) Controller access type
configurations, Gateway and metadata policy.

**Outputs:** Exposure Service and provider resources, SecuredAccess metadata
and status, including resolved endpoints. This scope does not author Certificates.

An unknown/disabled effective access type, or Route access without the Route API,
only produces an unsupported-type status: it authorizes no output creation,
update or cleanup. Existing Services and provider resources remain unchanged
until the type is corrected. Deleting the definition still permits owned-output
cleanup, and valid provider changes still replace obsolete exposure resources.

## SecuredAccess Certificate Scope

**Key:** Certificate namespace + name.

**Inputs:** SecuredAccess resources targeting the Certificate and their
resolved endpoints. The existing Certificate. Metadata policy.

**Outputs:** The shared Certificate spec, ownership and metadata.

**Preserves:** Combined hostnames and a stable subject when multiple
SecuredAccess resources share a Certificate.

## Gateway Scope

**Key:** Controller namespace + shared Gateway name.

**Inputs:** Gateway configuration, the existing Gateway and metadata policy.

**Outputs:** The shared Gateway spec and metadata. SecuredAccess scopes consume
its observed addresses but do not write it.

An existing Gateway normally requires
`internal.skupper.io/gateway-controller=<controller logical name>`. The sole
Gateway writer can automatically add that claim to a legacy Gateway only when
it is named `skupper` in the controller namespace, is not deleting, has no
owner references and has no existing claim annotation (an empty or different
claim is not absence). Its exact-version `gateway.networking.k8s.io/v1` Apply
history for field manager `skupper-controller` must cover `spec.gatewayClassName`
and the old `tls` listener's port, protocol, TLS mode and allowed namespace
fields. Matching names/specs, Update history or metadata-only Apply history do
not qualify. Configuration drift is reconciled normally; the current spec need
not equal the old template. Access scopes use the same eligibility check so
startup ordering does not temporarily reject an eligible legacy Gateway; only
the Gateway scope writes its annotation, using the observed resourceVersion and
non-forcing SSA. After success, the durable claim is the ongoing authority.

This is an explicit compatibility exception, not proof of original creation:
legacy code may have applied into an existing Gateway, and an SSA manager name
is not a security identity. Ambiguous resources remain blocked and require an
operator to inspect and explicitly authorize the specific Gateway. Do not
replace another claim or recreate the shared Gateway to obtain ownership. The
controller logical name must remain stable across restarts/upgrades; use an
explicit name when deployment discovery cannot provide that stability. This
transition does not fix partial Service/provider updates when authorization
fails for an ineligible Gateway.

## AccessGrant

**Key:** AccessGrant namespace + name.

**Inputs:** AccessGrant, Site, Controller grant-server URL/CA configurations,
time for initialization/expiry.

**Outputs:** AccessGrant status

## Common Inputs: Controller Context

Some controller scoped inputs are common to all scopes: namespace control,
labels and annotation policy, grants server configurations. These are packaged
as controller context and are snapshotted along with scope input. Changes to
controller context may trigger dependent scopes.

## Logging

The logging step at the end of each scope handler should follow a consistent
set of logging practices and fields. Handlers should avoid logging statements
outside of this dedicated step. In general, the logging step should reflect at
INFO (and above) the results of the actions the scope handler took. It should
answer the questions: **What did the handler do and why? Which resources were
involved? What may need attention?**. At the DEBUG level handlers include a
final diagnostic summary answering for developers **When did a specific scope
handler run? How many actions did it run? How many failed? Did it no-op?**.

* **Log Level:** INFO for successful writes, ERROR for failed attempts, DEBUG
  for diagnostic summaries. A "settled" pass logs nothing at INFO or above.
* **Fields:**

Fields that are always present on scope handler logs.

    * `component` an existing skupper standard loosely correlated with go
      package name.
    * `scope` e.g. Certificate, Site, etc. Required.
    * `key` The queue key. Required.
    * `outcome`: succeeded, failed, unchanged for actions. succeeded or failed
      for summaries.

Fields set on action logs.

    * `kind` target resource kind.
    * `namespace` target resource namespace. (when applicable)
    * `name` target resource name.
    * `action` create, update, delete, status.
    * `operation` optional domain operation e.g. "renew" for Certificate Secret
    * `reason` optional domain action reason e.g. "hosts-changed"
    * `duration` action execution time
    * `error` diagnostic on failure.
    * `conditions` confirmed written status conditions when action=status.

Fields on summaries

    * `phase` Last phase reached (in case of early exit): key, snapshot, plan,
      execute
    * `actions` count of actions executed
    * `failed_actions` count of actions with a failed result
    * `unchanged_actions` count of actions with an unchanged result
    * `changed_actions` count of successful actions not classified as unchanged
      (SecuredAccess/Gateway summaries)

Examples:

New Certificate: Create Secret, Update Status, Summarize
```
INFO msg="Created Secret east/api-tls for Certificate east/api-tls"
    component=kube.certificates.manager scope=Certificate key=east/api-tls
    kind=Secret namespace=east name=api-tls action=create outcome=succeeded duration=12ms
INFO msg="Updated Certificate east/api-tls status: Ready=True (Ready)"
    component=kube.certificates.manager scope=Certificate key=east/api-tls
    kind=Certificate namespace=east name=api-tls action=status outcome=succeeded duration=3ms
    conditions="[map[message:OK observed_generation:1 reason:Ready status:True type:Ready]]"
DEBUG msg="Reconciled certificate scope"
    component=kube.certificates.manager scope=Certificate key=east/api-tls phase=execute
    actions=2 failed_actions=0 unchanged_actions=0 outcome=succeeded
```

Settled Certificate: nothing needs changed
```
DEBUG msg="Reconciled certificate scope"
    component=kube.certificates.manager scope=Certificate key=east/api-tls
    phase=execute actions=0 failed_actions=0 unchanged_actions=0 outcome=succeeded
```

Expired certificate: renew Secret; keep existing Ready status
```
INFO msg="Updated Secret east/api-tls for Certificate east/api-tls"
    component=kube.certificates.manager scope=Certificate key=east/api-tls
    kind=Secret namespace=east name=api-tls
    action=update operation=renew reason=expired outcome=succeeded duration=15ms
DEBUG msg="Certificate east/api-tls status unchanged: Ready=True (Ready)"
    component=kube.certificates.manager scope=Certificate key=east/api-tls
    kind=Certificate namespace=east name=api-tls action=status outcome=unchanged duration=0ms
    conditions="[map[message:OK observed_generation:1 reason:Ready status:True type:Ready]]"
DEBUG msg="Reconciled certificate scope"
    component=kube.certificates.manager scope=Certificate key=east/api-tls phase=execute
    actions=2 failed_actions=0 unchanged_actions=1 outcome=succeeded
```
