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
the API server instead of predicting them locally. There is no runtime OpenAPI
schema discovery or client-side drift comparator. Other outputs retain their
minimal-update plans.

For these SSA outputs, settled means zero **stored mutations**, not zero PATCH
requests. Retain the observed resourceVersion as an update precondition and use
resourceVersion `0` for an absent/create plan. A successful update whose returned
resourceVersion equals that precondition is unchanged: log it at DEBUG and count
the attempted action in the summary. A different returned version is a stored
change, possibly ownership metadata or persisted defaults rather than a spec
repair; log the apply at INFO. Never classify a failure/timeout as unchanged, or
remove preconditions/force ownership to hide a conflict. Errors still reach the
retrying event processor even when status reporting succeeds.

Unchanged requests still incur API, SSA and applicable admission/validation work.
No stored mutation means no new API watch update, but periodic informer resyncs
still cause finite batches of applies. Admission that changes an object on every
update can cause a successful-write feedback loop; serial processing and stable
handler/key deduplication do not prevent such a sequential loop. DEBUG suppression
is a logging policy, not a loop-prevention mechanism.

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

Real-API tests must distinguish request counts from stable stored revisions at
settled state and after restart, and cover ownership-only changes, independent
fields, no-policy/empty metadata, policy chaining/retention, obsolete-owned-field
removal, stale-version/create races, conflicts and live schema evolution. Exercise
real informer/EventProcessor convergence between periodic resyncs, not only
manually refreshed handlers.

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
