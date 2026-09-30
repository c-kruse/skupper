# Proposal: namespace reconciliation and a router control protocol

Status: **Proposed**. This document describes a redesign of the Kubernetes
`controller` and `kube-adaptor`, called the **router adaptor** below. It does not
describe implemented behavior or redesign the non-Kubernetes controller.

## Requirements and recommended design

The controller reconciles namespaces by deriving desired state from two sources:
shared Kubernetes informer caches and observations received from router adaptors.
The router adaptor realizes intent against its local router and reports what it
accepted, what it verified as applied, and what the router is doing operationally.

The following are requirements for this redesign:

* Use an in-memory, namespace-keyed, deduplicating workqueue. Events invalidate
  scopes; a reconciliation collects a snapshot, purely derives desired state,
  plans changes, and executes eligible operations.
* The controller exclusively owns complete router intent. Replace router-config
  ConfigMap delivery with a controller service that sends a complete snapshot
  followed by incremental deltas, with content digests and refresh/recovery.
* An adaptor generates its RPC private key in process, submits a CSR with a
  mounted Kubernetes service-account token, and receives a short-lived client
  certificate. Normal router RPCs authenticate and authorize using mTLS identity.
* Router observations come from the **local router only**. Neither the controller
  nor the adaptor constructs a network-wide status map for reconciliation. Remove
  the `skupper-network-status` ConfigMap from this path.
* Stop populating `Site.status.network`. Drop `exposePodsByName` on Listeners and
  Connectors. Remove Connector `Matched`, `hasMatchingListener`, and the matching
  requirement from Connector readiness.
* Use router management APIs, not commands executed in the router container.
* Only the controller leader is Ready and serves adaptor traffic. Synchronize
  caches before becoming Ready. Exit on leadership loss. Favor less election
  churn over very fast failover; roughly 30 seconds is an acceptable target.
* **No observation is not an observation of empty state.** This distinction must
  survive startup, failover, disconnects, failed queries, and status aggregation.

Recommended choices below, rather than additional settled requirements, include:

| Concern | Recommendation |
| --- | --- |
| Reconciliation scope | Namespace, validated against namespace UID; Site UID identifies its active site's outputs. |
| Module boundaries | Separate site/bindings, access, and certificate derivation modules under one namespace executor. |
| Transport | TLS enrollment endpoint and an adaptor-initiated bidirectional gRPC stream, on separate ports of one controller Service. |
| Router intent | One complete logical snapshot per router target; domain resources rather than serialized QDR entities. |
| Content identity | SHA-256 over a versioned, normalized canonical JSON payload; explicit delta base/result digests. |
| RPC client certificates | Two-hour maximum lifetime with renewal near one hour using bounded jitter. |
| Leader election | Start with LeaseDuration 30s, RenewDeadline 20s, RetryPeriod 5s; measure end-to-end recovery. |
| Partial execution | Retain successful effects, block dependents, and retry from fresh observations. No general rollback. |
| Invalid traffic intent | Fail closed for that contribution; acquisition failures instead hold operations requiring missing evidence. |

## Two input caches feed one namespace reconciliation

```diagram
┌────────────────────────┐       ┌────────────────────────┐
│ Kubernetes informers   │       │ Router observation     │
│ CRs, Pods, outputs     │       │ cache: authenticated   │
└───────────┬────────────┘       │ reports + freshness    │
            │                   └───────────┬────────────┘
            └──────────────┬────────────────┘
                           ▼
                Map affected namespaces
                           │
                           ▼
                In-memory namespace queue
                           │
         ┌─────────────────┘
         ▼
┌────────────────┐     ┌────────────────┐
│ Snapshot       │────▶│ Derive         │
│ Effectful      │     │ Pure           │
└────────────────┘     └───────┬────────┘
                              ▼
┌────────────────┐     ┌────────────────┐
│ Execute        │◀────│ Plan           │
│ Effectful      │     │ Pure           │
└──────┬────┬────┘     └────────────────┘
       │    └─────────────▶ Kubernetes API → informers
       ▼
Complete intent publisher → mTLS stream → router adaptor
                                            │      ▲
                                            ▼      │
                                      Local QDR management

Router adaptor → mTLS reports → observation cache
```

The stream receiver validates and stores observations, then invalidates affected
namespaces. It never modifies desired intent or CR status directly. The intent
publisher distributes immutable output from the namespace executor. It cannot
independently add or remove router resources. Slow streams do not occupy workers.

Kubernetes still stores CRs, workloads, Services, credential Secrets, and small
bootstrap/trust inputs. Complete router intent and router observation inventories
are not serialized into ConfigMaps or copied into CR status. Public status is a
bounded projection of the observations relevant to that CR.

### What changes from the current implementation

The existing [EventProcessor](../../internal/kube/watchers/watchers.go) deduplicates
resource/handler keys; [ResourceWatcher](../../internal/kube/watchers/resources.go)
dispatches the latest cached object. [Site](../../internal/kube/site/site.go),
[ExtendedBindings](../../internal/kube/site/extended_bindings.go),
[CertificateManager](../../internal/kube/certificates/mgr.go), and
[SecuredAccessManager](../../internal/kube/securedaccess/access.go) retain additional
mutable state and incrementally update outputs. Unchanged-spec shortcuts do not
necessarily repair drifted output.

Today [ConfigUpdate](../../internal/kube/qdr/update_config.go) mutates the router
document in a ConfigMap. [ConfigSync](../../internal/kube/adaptor/config_sync.go)
reads it, materializes credentials, and calls QDR management. A successful sync
call does not establish application: [SyncBridgeConfig](../../internal/qdr/sync_router_ops.go)
can return nil while logging that the bridge configuration is not synchronized.

The adaptor's [collector](../../internal/kube/adaptor/collector.go) elects a site
collector and publishes global network information. The controller uses that to
populate topology, matching conditions, and pod-name exposure. Replace this
reconciliation dependency with reports from every local router adaptor. This
does not require removing independent network-observer/console products; those
must not become dependencies of this controller's readiness or desired intent.

## Namespace is the coordination and ownership boundary

Use namespace rather than Site name as the workqueue key. A namespace can contain
bindings before a Site exists, competing Sites, standalone Certificates and
SecuredAccesses, and cleanup after Site deletion. Preserve the established active
Site UID while it exists. If no owner is established and multiple Sites compete,
report a conflict rather than select one by informer order. Namespace or Site
recreation must not inherit an old UID's allocation or deletion authority.

Allow parallel reconciliation across namespaces, with one active attempt per
namespace. Execute operations sequentially within an attempt initially. The
existing `skupper` ConfigMap assignment still identifies the logical controller
responsible for a namespace. Losing assignment stops writes and router sessions;
it does not mean the namespace should be uninstalled.

### Events invalidate every affected namespace

Use shared caches per resource type/watch scope and namespace, reference, and
ownership indexes. A shared pod cache serves Connector selectors. Map both old
and new references and labels, including delete tombstones. If a tombstone is
insufficient, conservatively invalidate the bounded set of possible consumers.

| Changed resource or observation | Namespaces to enqueue |
| --- | --- |
| Site, Listener, MultiKeyListener, Connector, Link, RouterAccess | Resource namespace. |
| SecuredAccess, Certificate, relevant Secret, Service, Deployment, other managed children | Owner/resource namespace and indexed consumers. Include external inputs and objects causing name conflicts. |
| Pod identity, readiness, addresses, labels, or deletion | Its namespace and authorized site namespaces selecting its targets; include old matches. Router Pod lifecycle also invalidates authentication and observations. |
| AttachedConnector | Its namespace and old/new `siteNamespace` destinations. |
| AttachedConnectorBinding | Site namespace and old/new connector namespaces. |
| Adaptor acceptance, application, operational changes, disconnect, or observation expiry | Its authenticated Site namespace and source namespaces of affected attached definitions. |
| Namespace assignment/deletion or allocation changes | That namespace and affected dependents. Revoke execution/session authority on loss of control. |
| Templates, controller settings, supported capabilities | All affected namespaces; broad fan-out is acceptable initially. |
| Shared Gateway | Controller namespace, which owns it, and access-consuming namespaces. |

Filter on consumed fields, not just generation. Secret material, pod readiness,
assignment, and output drift matter. A repeated identical observation or unchanged
condition timestamp must not cause a reconciliation loop. Liveness messages update
session health, not management-sample freshness. Successful repeated samples can
advance freshness deadlines without a full derivation when facts are equal.

### Queue behavior

Use the `client-go` rate-limiting workqueue. Call `Add(namespace)` from invalidation
handlers. Repeated adds coalesce; an add during processing causes another pass
after `Done`. Always call `Done`; `Forget` resets retry history, not dirty work.

* Success: `Forget`, then `AddAfter` for the next domain evaluation if needed.
* Transient failure: capped exponential backoff with jitter and server retry
  guidance. Never abandon a namespace after a fixed retry count.
* Known dependency wait: events plus `AddAfter` fallback; no tight error loop.
* Invalid intent: stable diagnostics, relevant events, and slow audit. Failure
  to write those diagnostics is still a retryable effect failure.

Combine retry, renewal, and observation-expiry deadlines. New events may prompt
an earlier pass. A delayed add remaining after success is harmless because a
converged reconciliation performs no material writes.

At startup and leadership acquisition, wait for required cache/handler sync and
enqueue namespaces discovered from inputs, owned outputs, and allocation state.
Include cleanup-only namespaces. Periodic audits repeat enumeration and repair
missed notifications. Domain evaluation times are recomputed from resource state.

## Snapshots represent knowledge, not just lists

The conceptual interfaces remain:

```go
Collect(ctx, namespace) (Snapshot, error)
Derive(snapshot) DesiredNamespace
Plan(snapshot, desired) Plan
Execute(ctx, plan) ExecutionReport
```

The snapshot includes:

* Namespace identity/assignment; Site and local CRs; authorized attached joins;
  selected-pod candidates; managed outputs and relevant credential inputs.
* UIDs, generations, resourceVersions, ownership, allocation/lifecycle records,
  controller settings, capabilities, and one explicit evaluation time.
* Expected router targets and current router Pod identities from Kubernetes.
* Immutable copies of accepted/applied intent identities and per-resource local
  observations, with stream identity, router incarnation, completeness, and age.

Deep-copy informer data and take immutable observation-cache views. Pure functions
do not call clients, listers, clocks, random generators, credential providers, or
stream senders. Never log or serialize whole Secret-bearing snapshots.

Informer stores do not provide a transaction across kinds or with router reports.
Record input identities and reconsider on changes. Do not order resourceVersions
across objects. Revalidate critical intent, assignment, and ownership before
destructive operations; a changed prerequisite supersedes its dependent plan.
Use successful API responses or targeted reads for read-after-write dependencies.

| Input state | Meaning and permitted behavior |
| --- | --- |
| Required Kubernetes collection unreadable or unsynchronized | Snapshot acquisition failed. Do not infer absence or generate cleanup from it. |
| No router report, disconnected stream, stale sample, or failed management query | Router facts are Unknown. Continue deriving valid intent from Kubernetes; withhold observation-dependent success and retirement. |
| Complete fresh local inventory containing zero owned entities | Known empty router state. If intent requires entities, apply/repair them; if it requires absence, verify that absence. |
| Partial local inventory | Only explicitly covered facts are known. Omitted entities are not absent; finish or refresh the inventory. |
| Complete desired intent with no traffic resources | Explicit removal of owned traffic resources, while retaining bootstrap/control facilities. |
| Desired intent not yet derived for a target | IntentUnavailable. Never substitute an empty snapshot. |

**Unknown observations do not prevent initial intent delivery.** A router needs
intent before it can apply it or report application. A new controller can derive
and send intent before gathering reports; it simply cannot claim convergence yet.
Likewise, membership comes from authorized workload/Pod inventory, not successful
streams or Pod readiness alone, so a missing router cannot disappear from checks.

## Desired namespace state composes independent domain modules

`DesiredNamespace` contains Kubernetes objects/owned fields, complete router
intents, certificate issuance requests, port allocations, conditions, operation
dependencies, and explicit Ensure/Absent/Hold decisions.

1. **Site and bindings:** infrastructure, RouterAccess, listener Services, port
   assignment, local pod selection, Links, HA connectivity, and per-router intent.
2. **Access:** effective SecuredAccess definitions become exposure objects,
   endpoint facts, and certificate requests. Standalone use remains supported.
3. **Certificates:** combine explicit definitions and generated requests, validate
   issuers/usages/hosts, and request issuance or renewal. Cryptography is an effect.

A LoadBalancer's hostname can be unknown: derive its Service now and wait for its
address before issuing a certificate requiring that address. Generated RouterAccess,
SecuredAccess, and Certificate CRs remain inspectable. Pure composition can use
their planned specifications immediately, but ownerReferences require real UIDs
from successful creation or a subsequent snapshot.

Generate shared certificate requests from the sorted union of current consumers,
with owner UIDs as provenance. Removing an owner removes its contribution.
Incompatible requirements are conflicts. Never rewrite a user-managed Certificate
or Secret to satisfy generated requests. No manager calls another manager's
effectful `Ensure` during derivation.

| Output | Logical writer |
| --- | --- |
| User-authored CR specification | User/GitOps; controller reads it. |
| Generated CRs, Kubernetes managed fields, public status | Namespace executor, with ownership validation. |
| Issued router traffic credentials | Certificate issuance operation; external Secrets remain externally owned. |
| Complete router intent | Namespace executor; publisher only distributes its immutable result. |
| QDR entities, local credential materialization and profile versions | Router adaptor. |
| Shared Gateway | Controller namespace executor, not each consuming Site. |
| Controller RPC trust and issuing authority | Controller installation lifecycle, independent of Site reconciliation. |
| RPC client private key | The adaptor process that generated it. |

### Attached connectors cross namespaces without transferring ownership

An AttachedConnector in A names site namespace B. Its corresponding
AttachedConnectorBinding in B authorizes A and supplies the routing key. B joins
both declarations and selected pods in A. Both must exist and authorize the join.
Both namespaces must be managed by the same logical controller installation;
leader and standby replicas of that installation share the same identity. An
unassigned or differently assigned source contributes no endpoints or credentials
and produces an Error on the Binding, without blocking unrelated Site resources.
B's executor writes its router intent, Binding status, and authorized
AttachedConnector status using local application observations and source
identities. Revalidate source namespace UID and controller assignment before
publication and source status writes. Reject projections for an old UID/spec or
a moved binding; source namespace and assignment changes invalidate the join.

**The attachment mechanism remains supported.** Pairing an AttachedConnector with
its authorizing AttachedConnectorBinding is distinct from the Binding's current
`Matched` condition. That condition and `status.hasMatchingListener` describe
whether a Listener exists for its routing key in the network; they do not describe
whether the two attachment declarations correspond. Today the Binding's Ready
requires Configured and Matched, whereas AttachedConnector Ready requires only
Configured. Authorization, declaration pairing, and pod-selector checks remain.

Changing either reference invalidates old and new destinations and the source.
Deleting either declaration removes the contribution. Remove only the Binding's
network-listener `Matched` condition, `status.hasMatchingListener`, and that
readiness dependency, just as for Connector. Neither connector form needs a
remote Listener to be configured and ready. Listener-side destination matching
remains supported and uses local router observations.

Do not create cross-namespace ownerReferences or mutate A's resource specifications.
Preserve the authorized-controller scope. B's traffic credentials resolve in B;
permission to select pods in A does not authorize reading A's arbitrary Secrets.

### Stable allocations and credential lifecycles have distinct owners

Keep stable listener-port assignments in a small versioned allocation record,
keyed by Site UID and resource/subtarget identity. Inserting a Listener must not
renumber others. Reserve bootstrap/RouterAccess ports and detect exhaustion.
Commit an allocation before publishing dependent Services or intent. Unexpected
record loss blocks conflicting allocation; migration can explicitly import and
validate assignments from existing owned Services and router configuration.

Keep allocation data outside annotations, size-test it, and partition by stable
key if necessary. Do not turn it into a copy of the router intent. Time is explicit
for renewal and transition evaluation and is excluded from ordinary intent hashes.

Controller certificate operations reuse valid material. After a timeout, read the
Secret and validate its identity/requirements before generating again. Router-side
credential versions and QDR profile ordinals move to the adaptor's provider and
local realization state; they are no longer controller-authored intent fields.
Import existing profile tracking explicitly during migration.

## Router intent is a complete domain document

One `RouterIntent` describes all controller-owned behavior for a router target.
A target identifies namespace UID, Site UID, and router group/logical slot. A
particular Pod and router process are realizations of that target, not its sole
identity. Current HA groups remain separate targets. The schema can express
target-specific endpoints without requiring a node-router deployment in this work.

Compose common and target-specific contributions **before** publication. Do not
send independently mutable shared and per-node scopes that can temporarily omit
or duplicate a resource when it moves between scopes. Content sharing in the
publisher is an optimization of this one logical snapshot.

| Intent resource | Contents |
| --- | --- |
| Router settings | Site/group identity, routing mode, owned address policies, bootstrap requirements, and supported settings. |
| RouterConnection / RouterListener | Peer endpoint or bind intent, role/cost, TLS policy, credential reference; includes RouterAccess and HA connectivity. |
| ServiceListener | Bind address/allocated port, protocol, routing key(s), ordered priority/selection policy, observer and TLS policy. |
| ServiceConnector | Routing key, protocol, explicit selected endpoints, target identity, and client TLS policy. |
| CredentialBinding | Provider reference and required properties/usages, never private keys, certificate bytes, file paths, or QDR profile ordinals. |

Give each resource a stable ID derived from its owning CR UID, role, and subtarget.
Include explicit references so the adaptor can validate a complete graph before
acceptance. Map these IDs to deterministic local entity names and reverse-map
observations to them. Do not rely on mutable names alone or infer ownership from
every entity returned by management.

The adaptor compiles domain intent to QDR configuration and management operations.
The controller does not merge arbitrary old QDR entries into new intent. Bootstrap
management/health facilities have explicit protected ownership; they cannot be
removed by a traffic-resource deletion. Router drift is repaired from accepted
intent, not adopted as a new source of intent.

### Content digests identify content, not freshness or application

Define a versioned normal form and encode it with
[JSON Canonicalization Scheme, RFC 8785](https://www.rfc-editor.org/rfc/rfc8785).
Hash the complete normalized payload, including schema version and target, with
SHA-256. Use a domain prefix such as `skupper-router-intent/v1` in the hash input.
Specify exact prefix framing and publish interoperability test vectors.

Normal form must define defaults, absent versus empty values, and collection
ordering. Sort sets and resource IDs; preserve meaningful order such as routing
key priority. Reject duplicate IDs/JSON keys, unknown required fields, invalid
Unicode, and invalid numbers. Restrict numeric intent fields to bounded integers;
encode larger exact values as schema-defined strings. Do not hash timestamps,
transport sequences, source resourceVersions, RPC certificates, credential bytes,
or adaptor-local realization identifiers.

gRPC envelopes may use protobuf, but
[deterministic protobuf serialization is not canonical](https://protobuf.dev/programming-guides/serialization-not-canonical/).
Do not use raw protobuf bytes as the content identity. Compression and chunking
also do not change the digest: verify the reconstructed canonical content.

A digest says that two logical payloads are equal. It does not say which is newer,
which controller is authorized, whether the router applied it, or which credential
revision was loaded. Sessions and sequence numbers handle ordering; application
reports and realization identities handle those other facts.

## Enrollment establishes a narrowly authorized adaptor identity

Expose two ports on one internal controller Service:

* **Enrollment:** server-authenticated HTTPS, accepting a projected service-account
  token and CSR. This endpoint alone accepts bearer-token authentication.
* **Router control:** HTTP/2 gRPC with mandatory client certificates, carrying
  intent and observations. A bearer token is not sufficient on this endpoint.

Provision the controller server certificate, dedicated client issuing CA, and
public trust bundles as installation infrastructure. All controller replicas use
the same trusted issuing authority across leadership changes. Protect private CA
material in the controller namespace; do not make it available to router Pods.
Mount the service's public server trust bundle in adaptors and verify its DNS SAN.
This trust must exist before enrollment; the enrollment response cannot bootstrap
trust in an otherwise unverified server. Keep RPC trust separate from site/link TLS.

Enrollment proceeds as follows:

1. The adaptor generates a key in memory and signs a CSR. Its projected token has
   a dedicated controller-enrollment audience and is bound to its Pod. Reread the
   mounted file for each enrollment/renewal; kubelet rotates it.
2. The controller calls Kubernetes `TokenReview` with that explicit audience. Require
   authenticated success and a matching returned audience. Fail closed on API
   errors. A decoded JWT by itself is not authentication.
3. Validate the authenticated ServiceAccount UID, bound Pod UID, namespace, live
   Pod ownership chain, Site/group assignment, and expected ServiceAccount. Reject
   deleting/replaced Pods and arbitrary workloads using a similarly named account.
   Do not require Pod Ready: config-init must enroll before router startup.
4. Obtain Pod identity from verified bound-token information, not claimed request
   fields or CSR subject. Require supported bound-token identity validation on the
   supported Kubernetes versions. Node-local placement, if later used, comes from
   the authoritative Pod/Node objects, not unverified node metadata in a token.
5. Verify CSR signature/key parameters, then construct the certificate identity
   server-side: controller installation, namespace UID, Site UID, group, Pod UID,
   and ServiceAccount UID. Ignore requested identity SANs and reject CA/escalated
   usages. Issue client-auth-only credentials for at most two hours, bounded by
   issuer lifetime but not by the remaining bootstrap-token lifetime.
6. Return the certificate chain; the private key never leaves the adaptor or goes
   into a Secret, shared volume, config file, RPC response, or log.

The controller needs `create` permission for TokenReview and read access to the
identity/ownership objects; adaptors do not need TokenReview or CSR-approval rights.
Rate-limit enrollment, bound CSR/request sizes, and redact tokens and request bodies
from logs. Issuance is an effectful RPC operation, not part of pure site derivation.

At stream establishment, verify chain, validity, client-auth usage, and identity,
then authorize the current namespace/Site/Pod assignment. Bind the stream to that
target; reject message-level target substitution. Watch identity deletion and
assignment changes to close sessions. Bound authorization freshness during API
outages at certificate expiry and stop accepting reports/sending changes after
expiry. A missed watch can therefore leave issued access valid for up to two hours;
this is an explicit availability/security tradeoff, not immediate revocation.

Do not repeat the complete live Kubernetes authorization walk continuously for
connected streams. Each admission performs authoritative reads of the Pod,
ServiceAccount, Namespace, ReplicaSet, Deployment, Site, assignment, and allocation
objects. After admission, shared synchronized informers revoke only sessions
affected by authorization-relevant field changes. Synchronize both informer stores
and their authorization handlers before serving. A namespace-scoped generation
barrier spanning the admission reads and registry insertion prevents an event in
that interval from being missed. Pod status and allocation-port-only updates are
not authorization changes. Remember certificates canceled by a semantic watch
event until their signed expiry, so the same credential cannot reconnect after
watch-driven denial. New admission still repeats the complete authoritative check
and fails closed on API uncertainty. Do not run a periodic authorization auditor.
Watch failures are diagnostic; certificate expiry is the fallback when a semantic
revocation event is missed.

Renew near the certificate half-life with jitter using a freshly read projected
token, a new TokenReview, and a new in-memory key. Acquire and authenticate the
replacement while the old valid stream continues; transient enrollment failures do
not tear down usable control. An ordinary replacement stream then cancels the old
one. Preserve still-fresh operational evidence only for an overlapping replacement
with the exact target/Pod/ServiceAccount identity, router incarnation, accepted
intent, and realization. Preserve its original freshness deadline, clear pending
refreshes, and fence old-session callbacks. Close streams at certificate expiry;
TLS handshakes alone do not expire existing connections. Controller failover does
not rotate the CA.
Trust rotation needs overlapping bundles and an explicit rollout. RPC credential
expiry prevents control communication, not automatic deletion of live traffic
configuration. A compromised bearer token remains a bearer credential: use narrow
audiences, Pod binding, least privilege, and restricted mounts to limit exposure.

## One stream carries intent and local observations

Recommend a bidirectional `RouterControl.Sync` RPC initiated by the adaptor. Intent
and observations are independent message classes with separate sequences and
backpressure budgets. Use a single logical session per authenticated router Pod;
replacing it cancels the previous session. RPC methods do not mutate router state
directly on the controller's behalf; the adaptor reconciles complete accepted intent.

| Message | Essential fields/meaning |
| --- | --- |
| Hello / Welcome | Supported protocol/schema versions and capabilities, adaptor instance, observed router incarnation; server session ID and target authorization. |
| IntentUnavailable | This target has no publishable derived intent yet; retain current configuration and await a later message. |
| SnapshotBegin / Chunk / End | Session, desired sequence, schema/target, digest, bounded sizes, chunk order, and final completeness marker. |
| IntentDelta | Session, desired sequence, baseDigest, resultDigest, resource-ID upserts and explicit deletions. Large deltas use the same staged framing. |
| Accepted / Rejected / ResyncRequested | Validated content identity, failure scope/reason, or request for a full refresh. |
| ApplicationReport | Intent identity, router incarnation, resource/realization identities, convergence or precise pending/failure evidence. |
| ObservationBegin / Chunk / End; ObservationDelta | Complete named local observation scope, sample/base sequence, upserts/deletions, and query/completeness state. |
| Heartbeat / RefreshRequest | Session liveness, last sequences, and request to recollect a scope; not proof of successful router queries. |

Protocol negotiation happens before accepting content. An unsupported version or
required capability is explicit incompatibility, not an empty configuration. Keep
compatible old/new protocol versions during planned rolling upgrades. Cap both
encoded and decompressed sizes and validate all data before cache installation.

### Complete first, then base-checked deltas

1. Each new session receives a complete snapshot once that target has a publishable
   intent. The adaptor stages all chunks and validates target, schema, references,
   counts, and digest before atomically replacing its accepted logical document.
   Interrupted transfers leave its previous accepted document unchanged.
2. The controller sends subsequent deltas against the last **Accepted** digest,
   not against the last sent or Applied digest. An acceptance acknowledges receipt
   of a coherent document, not management convergence. Allow one unacknowledged
   intent transfer per session; coalesce later changes until acceptance or refresh.
3. The adaptor requires `baseDigest` to equal its accepted document, applies the
   complete delta to a copy, validates it, recomputes `resultDigest`, and then swaps
   the accepted document. A removal is explicit, never inferred from a partial delta.
4. Wrong base, gaps, unexpected duplicates, corruption, or unavailable sender history
   cause a full refresh. Recognize an exact duplicate transaction idempotently;
   never apply its operations twice. Reconnect begins a new session and full refresh.
5. Older-session frames and out-of-order desired sequences cannot replace newer
   content. Sequence numbers are scoped to the session; the controller's latest
   derived state is authoritative after a new session, not a numerical comparison
   between unrelated leaders' counters.

An accepted snapshot is coherent; QDR application is not transactional. A newer
accepted intent supersedes remaining work on an older target. Stop starting stale
operations, observe any in-flight outcome, and plan from actual local state toward
the new intent. An old application report cannot satisfy the newer target.

The server distinguishes **publisher initialized with explicit empty intent** from
**no derived result yet**. A missing target, unknown authorization, stream EOF,
timeout, or server restart never becomes an instruction to delete everything.
Only an authorized, complete intent can request deletion of owned resources.

### Bounded delivery rather than an unbounded stream backlog

Keep the newest desired snapshot and a bounded amount of accepted-base state per
session. Coalesce pending changes to the newest coherent target. If the base has
been evicted or delta cost exceeds a snapshot, send a full snapshot. There is no
requirement to deliver every intermediate intent.

Chunk transfers so the protocol is not constrained by ConfigMap size or one gRPC
message limit. Use compression, explicit decompression budgets, per-target/global
memory budgets, and fair scheduling. Sharing immutable encoded content across
identical targets is useful, but transmission still scales with recipient count.
Exceeding a configured safety budget produces a diagnostic; never truncate content
or claim the previous accepted document is current.

[gRPC flow control](https://grpc.io/docs/guides/flow-control/) is necessary but does
not bound application queues or prove receipt. Run independent send/receive loops,
bound staging, and prioritize acknowledgements and liveness over bulk inventories.
Do not block informer delivery or namespace workers on a slow adaptor. Coalesce
observations by entity; if a delta baseline is lost, resend a complete local scope.

## The adaptor compiles, applies, and observes its local router

Reuse the Go AMQP management client in
[internal/qdr](../../internal/qdr/amqp_mgmt.go), initially behind a narrow local
management interface. Extend typed decoding, cancellation, batching/pagination,
and read-back as needed. Missing/malformed attributes must not become zero counts
or empty collections through permissive conversion helpers.

Use only the local management endpoint. Do not call topology-walking helpers,
send management requests to remote router addresses, subscribe to a network-wide
VanFlow inventory, or use Kubernetes exec, `qdmanage`, shell commands, or container
command execution for apply or observation. A locally maintained routing table
is a local observation even when it reflects routes learned from other routers.

For each accepted intent, the adaptor:

1. Resolves credential references through its provider and validates requirements.
2. Compiles stable domain IDs to owned local entities and orders their dependencies.
3. Reads actual local entities and applies the required changes and removals.
4. Reads back required fields and verifies absence of removed owned entities.
5. Reports convergence for this intent and concrete realization; collects operational
   facts independently. A nil update call or successful file write is insufficient.

Management writes can partly succeed or time out after success. Retain successful
effects, reread ambiguous targets, and retry. Never prune prerequisites after failed
dependent application. Do not silently adopt unrelated management entities.
Settings requiring restart report RestartRequired with a bootstrap/config identity;
the controller plans a workload rollout. The adaptor does not restart the router
through an exec command. Preserve the goal of
[avoiding unnecessary router restarts](../adr/0002-avoid-router-restarts.md).

### Startup must not depend on operational observations

Adapt the existing [config-init](../../internal/kube/adaptor/config_init.go) path:
it uses enrollment and the full-intent RPC, resolves credentials, renders a complete
startup file, and atomically installs it in the existing shared configuration
volume. It then exits, allowing the router container to start normally. It cannot
report Applied because no router has been inspected yet.

The long-running adaptor generates its own in-memory RPC key, enrolls, opens a new
session, and obtains current intent before live reconciliation. There is no RPC
private-key handoff between init and sidecar processes. Router startup uses its
normal executable and generated file, not a command invoked remotely inside it.
Protect credential/config files with restrictive permissions and atomic replacement.

A new Pod may wait for the controller during an outage. Existing router processes
continue using their installed configuration while adaptors reconnect. Do not make
router Pod liveness/readiness depend solely on an active controller RPC stream;
that would turn a control-plane interruption into a traffic outage. An adaptor
restart invalidates application evidence until it rechecks the running router.

### Traffic credentials stay with the adaptor's provider

Use an adaptor-owned credential provider interface. The initial Kubernetes provider
resolves permitted Secret references, observes rotation, and maintains local files
and profile revisions. Restrict reads/watches to the necessary scope/references;
do not replicate the current all-Secret watch in every adaptor as the scale model.
The enrollment-audience token is not a Kubernetes API credential for these reads;
use a separately scoped API-audience token/mount where the provider needs one.

Credential bytes do not travel in router intent and rotations do not change its
digest. A provider change triggers local realization and new application evidence.
Use an opaque `realizationID` that changes when effective credential material or
other compiled dependencies change. Report non-secret provider revision/identity
and errors; never report keys or reusable secret material. A new router starts
from current valid credentials, not a controller-mandated historical ordinal.

Report credential rotation as a pending realization until verified. An old Applied
claim for the same intent digest cannot attest to newly observed provider material.
The controller compares dependency identity/revision where it knows that revision;
unknown provider progress remains explicit rather than being inferred from the digest.

Only the **RPC private key** is required to remain exclusively in process. Router
traffic TLS may need provider-managed files for QDR to load. Keep these two credential
lifecycles distinct. Retain old local traffic material until its live consumers are
gone; failed rotation must not delete working material or downgrade TLS. Explicit
revocation and confirmed invalid credentials follow the security policy below.

## Observations are local, complete by scope, and tied to realizations

Keep three independent facts:

* **Accepted:** the adaptor validated and installed the complete logical intent.
* **Applied:** local management read-back confirms the required owned configuration
  and removals for that intent and realization.
* **Operational:** current local behavior, such as a link being up or a listener
  accepting connections. Applied does not imply connectivity or healthy backends.

Every report is bound to authenticated Pod/target identity, adaptor instance,
router incarnation, session, sample sequence, intent digest, and relevant resource
IDs/realization IDs. Report failure/pending per resource; a document-wide Applied
claim requires every required resource and removal to be verified. Bind status to
the evaluated CR generation through the controller's derivation/provenance map.

Detect router restarts even within the same Pod. Use a management-exposed incarnation
where available; after management reconnection, conservatively invalidate prior
application evidence and establish a new observation epoch/read-back baseline.
An adaptor process restart likewise establishes a new baseline. Intent equality
does not make old application evidence current.

### Observation completeness and freshness

An observation snapshot names its scope, such as owned listener inventory or local
address reachability, and carries begin/end markers and sample identity. Install
it atomically after complete collection. A successfully collected empty scope is
KnownEmpty. Query failure, missing pages, truncation, or disconnect is Unknown for
the affected scope, with previous values retained only as last-known diagnostics.

Subsequent observation deltas use explicit base/result sequences and deletions.
On a gap, mark the scope uncertain and request a new complete report. Do not keep
using old positive evidence for cleanup or Ready merely because its last value was
true. A transport heartbeat does not refresh a failed management sample's age.

Use controller-timed refresh requests with a request ID and deadline to establish
freshness without assuming synchronized clocks. The adaptor samples after the
request and replies with a complete scope or a verified no-change result tied to
the installed sample sequence. Query failures cannot produce no-change success.
Unsolicited deltas update facts within the current freshness window; delayed
messages cannot revive an expired scope without a successful refresh. Bound query
and transfer duration, so buffering cannot make old evidence appear freshly sampled.

Disconnect invalidates current connection evidence; expiry enqueues the namespace.
Start with 10-second liveness/refresh intervals and a 30-second observation freshness
budget, independently configurable from election parameters and tuned against
collection cost. Large inventories must not silently make that budget impossible;
use bounded per-scope sampling and explicit uncertainty. Unchanged successful
refreshes need not retransmit the complete inventory or rewrite public status.

No router report updates another router's inventory. The new leader starts with
Unknown observations and requests current local snapshots from reconnecting
adaptors. It never reconstructs operational truth from old CR conditions.

### What the local management API can establish

| Resource | Local evidence and limits |
| --- | --- |
| Link / AMQP router connection | Local connector `connectionStatus`/`connectionMsg` and connection entities establish connection state. Peer metadata is reported only if locally available; no remote topology query. |
| TCP Listener | Owned `tcpListener` fields establish configuration; `operStatus` and `connectionMsg` describe the socket. An existing entity alone does not mean it accepts connections. |
| Listener destination | For the correctly mapped local routing address, `subscriberCount`, `inProcess`, and `remoteCount` describe the local routing view. Any supported consumer count above zero indicates a known destination, not proof of backend health. |
| MultiKeyListener | Local `listenerAddress` configuration plus per-key local reachability yields the reachable key set in intent priority order. The router makes actual selection; do not invent a current connection's chosen target from this set. |
| TCP Connector | Verify its configured endpoints/credentials. Observe supported activity/error counters; it is demand-driven and has no persistent connected/Matched requirement. Zero active connections is not Down. |

These fields are grounded in the router's management schema and listener address
watch implementation. Restrict address queries to routing keys used by this router's
intent rather than exporting all learned addresses or remote router identities.
Map internal address class/phase correctly; do not match raw string suffixes or sum
counts across unrelated address records. A missing record only means no locally
known destination after a successful, complete applicable query.

Validate this mapping against supported interior and edge-router versions. Where
a local management capability cannot expose the needed fact, report Unknown or
add a narrowly scoped local management field. Do not compensate with a global
collector. Some current byte counters are placeholders; unsupported counters are
unavailable, not fabricated zero-traffic measurements.

### Public status changes and compatibility

| Public behavior | New contract |
| --- | --- |
| `Site.status.network` | Stop populating; clear controller-owned legacy values during migration. |
| `Site.status.sitesInNetwork` | Stop populating/clear it too: an exact global count is incompatible with local-only observations. Do not substitute a local neighbor count. |
| Listener/Connector `spec.exposePodsByName` | Unsupported in this design. Remove generated per-pod-name routing/Services at controlled cutover. Pod selector targeting and `selectedPods` diagnostics remain. |
| Connector `Matched` and `status.hasMatchingListener` | Remove, including their role in Ready. Configuration and local dependency/application checks determine readiness, independent of remote listeners. |
| AttachedConnector / AttachedConnectorBinding pairing | Preserve both resources, the cross-namespace authorization join, and pod selection/configuration checks. |
| AttachedConnectorBinding `Matched` and `status.hasMatchingListener` | Remove only the network-listener-presence condition/field and its readiness dependency, not declaration pairing or authorization. |
| Listener `Matched` and `status.hasMatchingConnector` | Retain destination matching, derived from the local routing view rather than a global connector inventory. The condition expresses Unknown when evidence is unavailable; the boolean is a compatibility projection, not sufficient evidence by itself. |
| MultiKeyListener destination/reachable fields | Derive from local address observations. Do not publish an empty reachable list as a fresh fact when its observation is Unknown. |
| Link operational/peer fields | Local connection observations only. Retain only directly observed peer metadata; unavailable identity is omitted/unknown. |

For Listener status, local `router.address` consumer counts establish destination
availability, while `tcpListener.operStatus` establishes whether the socket accepts
connections. Keep these facts distinct: a destination can exist while a socket
fails to bind. Both inform readiness, and neither requires a network-wide status
map. These are observations of the running router, not inferred solely from its
desired configuration.

Retain legacy schema fields temporarily where needed for API compatibility, but
stop computing removed values and remove stale conditions. Reject new or changed
specs requesting `exposePodsByName`; do not silently reinterpret true as false.
Inventory existing uses and require operators to replace them before namespace
cutover. Update CRD schema/documentation, generated clients, CLI displays/waits,
and tests together. Public boolean/list fields cannot express uncertainty alone;
use an explicit observation condition and freshness/generation identity.

### Migration from `exposePodsByName`

`spec.exposePodsByName: true` previously created per-pod routing and Services.
It is not supported by the reconciled controller. Existing serialized fields are
retained temporarily so resources and clients can be decoded, but no new value
may request it and legacy `true` values must be removed before the namespace is
cut over. Replace the per-pod-name endpoint assumption with a normal Listener and
a Connector pod selector; use the Connector's `status.selectedPods` to inspect
the selected workloads. The controller does not reinterpret `true` as `false`, because that
would hide a behavior change.

Use Kubernetes conditions with True/False/Unknown appropriately. `Configured`
must distinguish intent publication from verified application; the proposal is
to set it from application evidence and describe Pending/Unknown explicitly.
Do not call receipt by the stream a successful configuration. Preserve condition
transition times when facts do not change; `observedGeneration` means evaluated,
not necessarily successful.

Aggregate required router targets explicitly. A proposed conservative rule is
that each required HA group has a current verified realization before reporting
the resource fully configured; a missing group remains Pending/Unknown even if
another group still serves traffic. Report partial availability separately.
During rollout, a warming replacement does not erase a verified serving realization,
but cleanup accounts for every old/new live consumer. Confirm this public HA
aggregation policy before implementation rather than changing it accidentally.

## Controller leadership gates both readiness and all effects

Run at least two controller replicas on separate failure domains when possible,
with explicit requests/limits. Standbys start and synchronize the same shared
informers without executing plans, enrolling adaptors, or serving router streams.
They can remain warm for takeover without holding router observations.

Use one Kubernetes Lease per logical controller installation. Proposed initial
parameters are LeaseDuration 30s, RenewDeadline 20s, RetryPeriod 5s. A hard-failure
takeover is approximately a lease interval plus acquisition, endpoint propagation,
and reconnect time. A genuinely cold process also needs cache sync. **30 seconds
is a tuning target, not a guaranteed bound under API/network failure.**

On acquiring leadership:

1. Confirm required caches/handlers are synchronized and authorization/trust state
   is usable. Begin a new serving session generation and initialize the publisher
   and observation cache. Bootstrap namespace work.
2. Enable execution and RPC handling, then become Ready. Per-target intent remains
   IntentUnavailable until derived; absent router observations remain Unknown.
3. Accept adaptor reconnects and complete local observation refreshes. Do not wait
   for all adaptors before readiness, which would prevent them reaching the Service.

Readiness is exactly active leadership plus initialized serving prerequisites.
The adaptor Service selects only Ready endpoints and does not publish not-ready
addresses. Use a separate health endpoint for process liveness/startup and an
authenticated diagnostic indicating cache-synced standby state; standby health
must not make it eligible for adaptor traffic.

On leadership loss, immediately fail readiness, close the enrollment/stream gates,
cancel plans, stop initiating Kubernetes/QDR-directed effects, close existing
streams, and **exit the process**. Do not remain alive as a former leader. Kubernetes
restarts it as a standby. On graceful shutdown, stop work and streams before
releasing the Lease; never release authority while effects can still start.

Service endpoint updates are asynchronous and established TCP streams are not
migrated by readiness. Server-side leadership checks and connection closure are
therefore mandatory. Adaptors use bounded RPC deadlines/liveness checks and
jittered reconnect to the stable Service, not a remembered leader Pod address.
Bind messages to the new session and reject late messages from prior sessions.

Kubernetes leader election is not strict storage-level fencing. Process exit,
cancellation, session replacement, and UID/resourceVersion checks limit overlap;
an already issued API or router management operation can still complete. Reobserve
and converge rather than promise atomic/exactly-once failover. If strict fencing
is required, it must be enforced at the write boundary, not inferred from a digest.

### Leader-only readiness requires a compatible rollout procedure

A normal multi-replica Deployment rollout expects replacement replicas to become
Ready. Standbys deliberately never do, so simply changing its readiness probe
can strand a rollout. Proposed packaging: a two-replica StatefulSet with
`podManagementPolicy: Parallel` and `updateStrategy: OnDelete`, without PVCs, plus
an explicit leader-aware replacement procedure in installation/upgrade tooling.
Use a separate governing Service; adaptors use only the Ready-filtered RPC Service.

Update/recreate a standby first, verify its startup/cache-sync diagnostic, then
terminate the leader and wait for the upgraded replica to acquire leadership and
serve. Recreate the former leader with the new template. Recheck Lease identity
before each step and abort if no healthy successor exists. Tests and rollout
health checks expect one Ready leader, not every replica Ready. OnDelete makes
this upgrade procedure required, not optional operational advice.

The feature branch supplies `go run ./cmd/controller-rollout --namespace <ns>`
for this procedure after applying an updated StatefulSet template. It checks the
current Lease against the Ready Pod's UID, replaces the old standby first, waits
for the upgraded standby's startup probe/cache synchronization, and only then
replaces the old leader. Pod deletions use UID and resourceVersion preconditions.
It rechecks leadership before deletion and exits on a changed holder or template;
these checks reduce races but do not make cross-resource reads transactional.
Completion requires two updated replicas and one Ready lease holder, not two
Ready Pods. The holder identity format is `<controller-name>/<pod-name>/<pod-uid>`;
it is stable across container restarts and distinct for replacement Pods, without
a transient random identity. Namespace assignment continues using the stable
controller namespace/name.

Set `SKUPPER_CONTROLLER_NAMESPACE` when generating manifests for an installation
outside the default namespace. Namespace-scoped installs also require the included
cluster-scoped TokenReview grant; a namespaced Role cannot grant this permission.
Converting an existing Deployment installation to this StatefulSet is a separate
cutover, not an in-place workload-kind update. Do not leave the legacy Deployment
running alongside the new controller authority.

Add topology spreading/anti-affinity and a disruption budget protecting the one
Ready leader. Since PDBs do not orchestrate handoff and direct deletions bypass
them, upgrades and node maintenance must use the same checked handoff procedure.
The standalone and Helm manifests use this leader-only serving contract.

## Plans and errors preserve successful work without guessing

Plans contain typed operations such as ApplyObject, CommitAllocations,
EnsureCertificateMaterial, PublishRouterIntent, UpdateStatus, and DeleteOwnedObject.
Each has target identity, managed fields, preconditions, dependencies, and failure
scope. They apply to one snapshot/attempt. Ensure/Absent/Hold are explicit; an empty
return caused by an error is never a desired empty resource set.

Return per-operation results in the attempt's `ExecutionReport`: Succeeded, Failed,
UnknownOutcome, SkippedDependency, or Superseded. Preserve the distinction between
a failed resource change, pending router application, and a failed status update.

Commit allocations and necessary Kubernetes prerequisites before installing the
publishable intent in the in-process publisher. Publication means the controller
has made that target available for delivery, not that an adaptor accepted/applied
it. Later observations enqueue another pass. Do not hold a namespace worker while
waiting for an RPC acknowledgement, a load balancer, or router convergence.

Dependencies gate destructive steps: only move a Service to a new allocated port
when the relevant serving router realization has applied it; only retire resources
when old consumers are accounted for. Independent work may proceed after another
branch fails. Server-side apply is suitable for owned Kubernetes fields; never
force ownership/adoption or immutable-field replacement as an automatic fallback.

| Failure | Execution and recovery policy |
| --- | --- |
| Incomplete Kubernetes snapshot | Retry acquisition; no assumed-empty plan. |
| Invalid spec or incompatible declarations | Report Invalid/Conflict, exclude invalid traffic intent, continue independent valid work. |
| Missing dependency or unresolved endpoint | Waiting; create repair prerequisites, wake on events plus a delayed check. |
| Kubernetes timeout, 429, 5xx | Retain successful effects, treat ambiguous writes as UnknownOutcome, re-read identity/content before retry with backoff. |
| resourceVersion conflict or replaced UID | Supersede stale dependent work and replan. Never replay deletion against a replacement UID. |
| Admission/RBAC/ownership failure | ActionRequired; stop that branch, retry slowly because external policy may change. No adopt/force fallback. |
| Enrollment/token-review failure | Do not issue credentials or accept unauthenticated reports. Retry transient API failures separately from invalid identity. |
| Stream disconnect, certificate expiry, or reconnect | Retain accepted intent and installed configuration; local repair/credential rotation can continue. Controller observations become uncertain; renew/reconnect and refresh intent/reports. |
| Wrong delta base, gap, or digest mismatch | Discard staged change, retain accepted document, request full refresh. Repeated malformed content is a protocol fault. |
| Transfer or resource budget exceeded | Explicit scoped diagnostic/backpressure; retain accepted content, never truncate or publish an empty replacement. |
| Adaptor rejects schema/capability/intent | No application claim; hold dependent controller operations, report a scoped incompatibility/error. |
| Partial/uncertain local management apply | Re-read actual owned state and retry toward latest accepted intent; retain dependencies and successful effects. |
| Failed/stale/incomplete local query | Unknown for the affected scope; retry observation, no absence-based deletion or false readiness. |
| RPC Accepted but not Applied | Pending application, not Configured. Status/cleanup wait for read-back, while unrelated work continues. |
| Status write fails after resources/intent changed | Keep those changes, retry status from current observations. Do not undo publication or reissue certificates. |
| Lost assignment | Cancel the namespace plan and sessions; new authorized controller reconciles. Do not uninstall. |
| Lost leadership | Withdraw readiness, cancel work/streams, and exit; successor bootstraps and gathers reports. |
| Internal invariant failure | Publish no invalid intent; stop the affected attempt, alert with bounded diagnostics, and retry with slow backoff. |

If a Service and Certificate are created and the stream disconnects after intent
delivery, the next pass retains those prerequisites. The adaptor might already
have applied the intent. Fresh read-back determines that; lack of acknowledgement
does not justify deleting resources or manufacturing a rollback.

### Invalid intent and unavailable observations are different policies

The proposed default is fail-closed for a malformed or revoked traffic contribution.
Removing an AttachedConnectorBinding revokes its contribution. A known missing or
invalid credential must never produce a plaintext fallback. Keep issuance resources
needed to repair it. Invalid Site-wide settings hold site infrastructure rather
than requesting uninstall. Flag existing `exposePodsByName` uses before migration,
not as an unannounced traffic deletion during upgrade.

By contrast, a failed API read, unavailable credential provider, controller outage,
or missing local report is not proof of invalid intent or an empty router. Hold
operations needing that evidence and retain last installed traffic configuration.
An unreachable adaptor cannot instantly apply a security revocation; report it as
unconfirmed, rather than claiming stream publication enforced it. Immediate
isolation would require a separate Kubernetes/network enforcement action.

Fail-closed invalid updates can interrupt traffic and remain a product decision to
approve. A last-known-good policy would need explicit accepted-intent retention and
revocation rules; arbitrary old router entries are never that policy.

### Retirement depends on real consumers

Controller cleanup uses ownership labels, owner UIDs, complete inventory, and
deletion preconditions. Use Kubernetes garbage collection for ordinary owner trees;
add finalizers only for cleanup requiring the parent's continued existence. Never
delete standalone user CRs or unrelated Secrets/Services because a Site disappears.

Track every serving router Pod during rollout. Unknown or failed application does
not release old port allocations or shared dependencies. A Pod confirmed gone is
no longer a consumer. The adaptor owns local profile retirement; controller-owned
shared credential resources still need consumer-aware deletion. Application
acknowledgements alone do not retain old key bytes or prove every connection drained.

Availability-preserving transitions may require explicit phases: publish overlapping
old/new endpoints, verify application, switch the Service, observe the switch, then
retire the old endpoint. Store necessary phase/allocation state, derive each complete
intent from it, and avoid reconstructing transitions from arbitrary QDR fragments.
Where overlap is impossible, state the disruption; neither a coherent snapshot nor
RPC gives atomic site-wide application. Initially avoid retired-port reuse until
the consumer/transition rules are implemented and tested.

## Keep grant redemption separate from router enrollment

AccessToken redemption can consume a remote AccessGrant and create credentials.
It is not the service-account-token/CSR exchange and is not a repeatable namespace
upsert. Keep its UID-keyed workflow outside pure derivation; its Secret/Link outputs
become normal snapshot inputs. Grant serving can use the access module for exposure.

An uncertain remote redemption response cannot be repaired merely by repeating
the POST. Safe retries need an idempotency key and replay of the same response by
the grant server. That protocol remains a separate follow-up; the router-control
redesign must not move this operation into generic reconciliation retries.

## Implement and migrate through explicit ownership handoffs

This is a coordinated controller/adaptor/API change, not a replacement ConfigMap
serializer. Work within existing ownership areas; reuse generated clients,
resource templates, access builders, and the local QDR client where appropriate.

1. **Specify contracts and fixtures.** Version domain intent, canonicalization,
   report scopes, identities, and public status semantics. Test interior/edge local
   reachability and read-back capability against supported routers.
2. **Extract pure namespace derivation.** Add shared caches, cross-namespace mapping,
   the workqueue, explicit uncertainty, and port allocation import. Keep existing
   execution while comparing full derived intent/compiled QDR semantics in shadow.
   Shadow work writes no resources and redeems no grants.
3. **Build controller HA and enrollment.** Install Service/trust, TokenReview RBAC,
   leader-only serving/probes, checked rollout, and expiry/authorization behavior.
   Verify takeover before making traffic configuration depend on the RPC service.
4. **Build the adaptor protocol and local reconciler.** Implement full transfer,
   delta/hash recovery, startup file generation, provider-local rotation, typed
   QDR application/read-back, and complete local observation reports.
5. **Update public contracts and consumers.** Remove network/matching/exposure
   dependencies from CRD status logic, generated APIs, CLI/console consumers, docs,
   and tests. Surface unsupported existing pod-name exposure before migration.
6. **Cut over a namespace deliberately.** Stop legacy mutation and collection for
   that scope, import validated allocations, and roll compatible adaptors. Each
   router Pod uses exactly one configuration source; never run legacy ConfigMap
   sync and RPC reconciliation against the same router. Account for old/new serving
   Pods during rollout before enabling application-dependent cleanup.
7. **Remove obsolete transport/state.** After no consumers remain, remove router
   configuration and network-status ConfigMaps, their watches/writes/RBAC, global
   controller aggregation, and obsolete profile-ordinal coordination. Keep needed
   credential and trust objects. No per-router status ConfigMap replaces them.

Migration can use explicit namespace assignment/version markers. Record allocation
schema and execution ownership. New status must not merge old network topology.
Old serving Pods without application evidence cannot satisfy new cleanup gates.

Define the rollback window before shipping. Reverting to a ConfigMap-based adaptor
requires an explicit conversion of representable current intent and compatible
credentials, stopping the RPC writer and rolling Pods; it is not just a controller
image downgrade. Do not maintain two live authorities as a rollback mechanism.

## Verification must exercise protocol and failure boundaries

Pure tests cover canonical content, stable ports, composed target intent, ownership,
standalone certificates/access, attached joins, and unknown versus known-empty
observations. Use property-based tests for input permutations, snapshot non-mutation,
idempotence, and unrelated additions preserving allocations. Independently specify
expected routing facts and hash vectors; do not derive expectations from the builder.

Protocol and executor tests must include:

* Initial IntentUnavailable versus explicit empty intent; missing reports versus a
  successful empty inventory; partial scope versus complete scope; query errors
  after a prior positive result; buffered stale samples and failed refresh deadlines.
  No uncertainty-driven destructive configuration or false observation freshness.
* Full snapshots larger than 1 MiB, multi-chunk transfer interruption, decompression
  limits, duplicate IDs, default/Unicode/order canonicalization, and version mismatch.
* Delta to the accepted-but-not-applied base, dropped/reordered/duplicate frames,
  wrong base/result digests, reconnect after sender state loss, and coalescing while
  older management operations partially succeed. No drift or stale acceptance.
* Applied read-back including removals, timeout after management success, router
  restart in the same Pod, adaptor restart, and credential rotation with unchanged
  intent digest but a different realization. Old evidence must fail current checks.
* Wrong token audience, expired/deleted-Pod token, wrong ServiceAccount/owner/Site,
  forged CSR SANs, cross-target RPC, TokenReview outage, certificate expiry on a
  long-lived stream, CA rotation, and authorization changes during a session.
* Connector readiness with no remote Listener or active TCP connections; local
  Listener/MultiKeyListener reachability on interior and edge routers; unavailable
  capabilities and placeholder counters; clearing old matching/network conditions.
* Repeated invalidations, an event around `Done`, delayed duplicate adds, API write
  success followed by lost response, status-only failure, and startup cleanup after
  inputs vanished. Independent namespaces/branches must continue converging.
* AttachedConnector reference moves, binding revocation, namespace/Site recreation,
  and stale projected status. Remote matching must not reappear through attachments.
* Leader death/partition, renewal delay, endpoint propagation delay, established
  old streams, cold cache synchronization, leader exit, reconnect storms, and rolling
  replacement with one Ready replica. Readiness must not wait for router reports.
* Existing forwarding during controller/RPC outage, blocked new-router startup,
  partial HA convergence, Service transitions, and complete consumer accounting
  before shared dependency deletion or allocation reuse.

Use a real API server for TokenReview, projected-token lifecycle, SSA/defaulting,
UID/resourceVersion checks, Lease behavior, probes, endpoints, and cache lag. Use
real routers for local observations, compiled-intent read-back, rotations, and
traffic-preserving updates. Fake clients and a nil sync return prove neither.

Scale tests vary services per router, routers per controller, selected endpoints,
large shared intent updates, status churn, and simultaneous reconnect/renewal.
Measure CPU, heap, encoding/hash cost, stream bytes, local query load, namespace
latency, application lag, and takeover time. Replacing ConfigMaps removes etcd
transport pressure, not fan-out, hashing cost, or router resource limits.

Expose bounded metrics for queue/retry behavior, RPC enrollment failures, session
counts, snapshot/delta bytes, resync reasons, observation age/completeness, and
accepted-to-applied lag. Keep resource identities in structured logs rather than
unbounded metric labels. Provide redacted intent/plan diagnostics and local health
summaries. Never log enrollment tokens, keys, or complete credential inputs.

Acceptance means convergence and quietness: stable workloads/status stop being
rewritten, no global status collection is needed, drift repairs locally, retries
cannot strand work, and failover does not erase working router configuration.

## Choices to settle before implementation

The requested feature removals, local-only reporting, full-plus-delta delivery,
in-memory namespace queue, ephemeral adaptor RPC keys, and leader-only serving are
not alternatives in this proposal. Remaining choices are:

* gRPC/schema details and canonicalization test vectors, supported router versions,
  report scope sizes, and measured transport/observation budgets.
* Certificate lifetime/renewal defaults, installation trust rotation, and supported
  Kubernetes bound-token identity behavior.
* Leader-only-readiness packaging and upgrade tooling, plus measured election and
  reconnect settings appropriate for the intended scale.
* Exact public HA aggregation and local Listener/MultiKeyListener condition wording,
  and the compatibility period for legacy status/spec fields.
* Fail-closed behavior for invalid traffic updates and the supported availability
  guarantees for port/credential transitions and rollback.

## References

* [Related router-intent discussion](https://ampcode.com/threads/T-01a0e94c-b0f6-7781-a7ca-63c73f4d07fb)
* [Controller startup and dispatch](../../internal/kube/controller/controller.go)
* [Current matching-status aggregation](../../internal/kube/site/binding_status.go)
* [Current public types and conditions](../../pkg/apis/skupper/v2alpha1/types.go)
* [MultiKeyListener status](../../pkg/apis/skupper/v2alpha1/multikeylistener_types.go)
* [Existing listener port recovery](../../internal/qdr/port_mapping.go)
* [Router Pod startup and mounts](../../internal/kube/site/resources/skupper-router-deployment.yaml)
* [Certificate owner aggregation](../../internal/kube/certificates/ownermappings.go)
* [Current token redemption](../../internal/kube/grants/redeem.go)
* [Router management schema](https://github.com/skupperproject/skupper-router/blob/main/python/skupper_router/management/skrouter.json)
* [Local listener reachability and selection](https://github.com/skupperproject/skupper-router/blob/main/src/adaptors/adaptor_listener.c)
* [Kubernetes bound-token verification](https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/)
* [Projected token audiences and rotation](https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/)
* [StatefulSet Parallel and OnDelete behavior](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)
* [Kubernetes API consistency/update semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/)
* [client-go queue dirty/processing contract](https://github.com/kubernetes/client-go/blob/v0.33.0/util/workqueue/queue.go)
* [client-go informer consistency contract](https://github.com/kubernetes/client-go/blob/v0.33.0/tools/cache/shared_informer.go)
* [client-go leader-election limitations](https://github.com/kubernetes/client-go/blob/v0.33.0/tools/leaderelection/leaderelection.go)
