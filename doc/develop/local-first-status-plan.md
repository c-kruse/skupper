# Local-first router status: implementation plan

Status: draft for review. Companion ADR (0012) to be extracted from the
Design section once the plan is agreed.

## Why

Today every Skupper site publishes a whole-network status document:

* The kube-adaptor (leader across all router groups, via the
  `skupper-site-leader` lease) runs `flow.StatusSync`
  (`internal/flow/status.go`). It discovers every vanflow event source in the
  network, stores SITE, ROUTER, LINK, ROUTER_ACCESS, CONNECTOR, LISTENER and
  PROCESS records from all of them, and serializes the whole thing as
  `network.NetworkStatusInfo` into the `skupper-network-status` ConfigMap
  (`internal/kube/adaptor/collector.go`).
* The controller watches that ConfigMap (`controller.networkStatusUpdate`),
  converts it into `[]v2alpha1.SiteRecord` (`network.ExtractSiteRecords`) and
  writes it verbatim into `Site.status.network`
  (`Site.NetworkStatusUpdated` in `internal/kube/site/site.go`). From the same
  payload it derives Link `Operational`, Listener/Connector `Matched`
  (`binding_status.go`), MultiKeyListener reachable keys, and
  `exposePodsByName` targets (`per_target_listener.go`).
* nonkube does the same with a file-backed ConfigMap
  (`internal/nonkube/flow/collector.go`,
  `internal/nonkube/controller/network_status_handler.go`,
  `pkg/nonkube/api/site_state.go`).

This scales as O(network) per site: every site stores and re-publishes every
CONNECTOR/LISTENER/PROCESS record in the network, then copies the result into
every Site object's status. Any change anywhere fans out to N ConfigMap writes
and N Site status writes.

## Design

### Principles

1. **Status is local.** Each router deployment group publishes what *its*
   router knows, gathered from the router's management API. The router is the
   authority on whether a link is up or a listener has a destination; vanflow
   is only consulted for facts the local router cannot know (which site a peer
   router belongs to, how many sites exist).
2. **One ConfigMap per router group, gzip-compressed, never chunked.**
   Preserves the Kubernetes API as the intermediary between adaptor and
   controller, with the same size discipline as ADR 0011.
3. **Status is inferred from configuration.** Every entity the controller put
   in the router configuration gets an observed state in the status document.
   The controller does not ask for status; the adaptor publishes it for
   everything it applies.
4. **The controller decides.** The adaptor publishes facts per group; the
   controller merges groups (HA) and maps facts onto CR conditions.

### Router status document

New package `pkg/skrouter/status` (platform neutral, depends on
`pkg/skrouter/mgmt` from `origin/add-skrouter-management-client`).

```go
// Document is the observed state of one router deployment group.
type Document struct {
    Version int    `json:"version"` // schema version, starts at 1
    Group   string `json:"group"`   // "skupper-router", "skupper-router-2", nonkube: "skupper-router"

    Router  Router  `json:"router"`
    Applied Applied `json:"applied"` // which configuration this document reflects

    Links         []Link         `json:"links,omitempty"`
    RouterAccess  []RouterAccess `json:"routerAccess,omitempty"`
    TcpListeners  []TcpListener  `json:"tcpListeners,omitempty"`
    TcpConnectors []TcpConnector `json:"tcpConnectors,omitempty"`
    Addresses     []Address      `json:"addresses,omitempty"`
    Prefixes      []PrefixQuery  `json:"prefixes,omitempty"`

    Network Network `json:"network"`
}

type Router struct {
    ID       string `json:"id"`       // router.id
    Mode     string `json:"mode"`     // interior | edge
    Version  string `json:"version"`  // router.version
    Hostname string `json:"hostname"` // pod name
}

// Applied identifies the configuration revision the adaptor last synced to
// the router, and whether that sync succeeded.
type Applied struct {
    ResourceVersion string `json:"resourceVersion,omitempty"` // router config ConfigMap
    Error           string `json:"error,omitempty"`
}

// Link is an inter-router or edge connector. Name equals the Link CR name.
type Link struct {
    Name             string `json:"name"`
    Role             string `json:"role"`                    // inter-router | edge
    Present          bool   `json:"present"`                 // connector entity exists in the router
    ConnectionStatus string `json:"connectionStatus"`        // connector.connectionStatus: CONNECTING | SUCCESS | FAILED
    Message          string `json:"message,omitempty"`       // connector.connectionMsg
    RemoteRouterID   string `json:"remoteRouterId,omitempty"` // connection.container
    RemoteAccessID   string `json:"remoteAccessId,omitempty"` // connection.properties["qd.access-id"]
    RemoteSiteID     string `json:"remoteSiteId,omitempty"`   // via Network.Routers
    RemoteSiteName   string `json:"remoteSiteName,omitempty"`
}

// RouterAccess is an inter-router/edge listener. Name equals the RouterAccess-
// derived listener name.
type RouterAccess struct {
    Name    string   `json:"name"`
    Role    string   `json:"role"`
    Present bool     `json:"present"`
    Peers   []string `json:"peers,omitempty"` // container ids of open incoming connections
}

type TcpListener struct {
    Name       string `json:"name"`      // "listener/<cr-name>" (or per-target name)
    Address    string `json:"address"`
    Present    bool   `json:"present"`
    OperStatus string `json:"operStatus"` // up | down
    Message    string `json:"message,omitempty"` // tcpListener.connectionMsg
}

type TcpConnector struct {
    Name    string `json:"name"`
    Address string `json:"address"`
    Host    string `json:"host"`
    Port    string `json:"port"`
    Present bool   `json:"present"`
}

// Address reports reachability of a routing key referenced by a local
// tcpListener or listenerAddress (multi-key listeners).
type Address struct {
    Name      string `json:"name"`
    Reachable bool   `json:"reachable"` // subscriberCount + remoteCount + inProcess > 0
}

// PrefixQuery is the result of an adaptor-config address prefix query.
type PrefixQuery struct {
    Prefix    string   `json:"prefix"`
    Matches   []string `json:"matches"`   // reachable addresses with the prefix, sorted, <= MaxPrefixMatches
    Truncated bool     `json:"truncated"` // more than MaxPrefixMatches existed
}

const MaxPrefixMatches = 128

// Network is the minimal vanflow-derived view.
type Network struct {
    Sites   []Site   `json:"sites"`
    Routers []RouterRef `json:"routers,omitempty"` // router id -> site id, used to fill Link.RemoteSite*
}
type Site struct {
    ID, Name, Namespace, Platform, Version string
}
type RouterRef struct {
    ID     string `json:"id"`
    SiteID string `json:"siteId"`
}
```

What is deliberately excluded: counters (bytes, connections opened, deliveries)
and anything else that changes without a topology change. The document must be
stable when nothing meaningful changed so publishing is change-driven.

### How each field is sourced

Verified against the skupper-router sources in
`/home/user/workspace/repos/skupper-router`:

| Fact | Source |
|---|---|
| Link connected / error | `connector` entity: `connectionStatus` (CONNECTING/SUCCESS/FAILED), `connectionMsg`. Today we get TLS and DNS failures only from logs. |
| Link remote router | `connection` entity, `dir=out`, `role` inter-router/edge: `container` is the remote router id. |
| Link remote RouterAccess | `connection.properties["qd.access-id"]` — the router copies the remote listener's vflow identity into connection properties (`qd_connection.c:177`, exposed via `connection.properties`, `amqp_adaptor.c:1590`). |
| Link remote site | vanflow ROUTER record named `0/<router id>` (`vanflow.c:_vflow_create_router_record`) → `parent` = site id → SITE record. Only SITE and ROUTER records are needed. |
| Listener matched | `tcpListener.operStatus`. The router only opens the socket when at least one of the listener's addresses has local, in-process or remote consumers (`adaptor_listener.c:440-460`). `up` is exactly "a connector for this routing key is reachable". |
| Per-address reachability (multi-key) | `router.address` named `M<address>`: `subscriberCount + remoteCount + inProcess > 0`. Works on edge routers for addresses the router has a listener/watch on: the edge proxy binds a link to the address when the interior reports destinations (`addr_proxy.c:585`). |
| `exposePodsByName` targets | Prefix scan of `router.address` for `M<routingKey>.`. Interior routers hold the full mobile address table (mobile_sync). Edge routers do **not** (`addr_proxy.c` only proxies addresses with local interest), so on an edge router the adaptor issues the query to its uplink interior via `_topo/0/<interior>/$management` (`mgmt.Client.Interior(id)`), where the interior id is the `container` of the open `dir=out, role=edge` connection — exactly what `qdr.GetInteriorAddressForUplink` does today. |
| Sites in network | vanflow SITE records. |
| Configuration applied | The adaptor's own `ConfigSync` knows the ConfigMap resourceVersion it last applied and the error, if any. Presence per entity comes from the same `Query` used by `reconcile.Sync`. |

Connector "has matching listener" has **no local source**: a tcpListener does not
subscribe or otherwise advertise itself to remote routers. This is why the
Connector `Matched` semantics are deprecated rather than reimplemented.

### Adaptor configuration for prefix queries

The router rejects unknown sections in `skrouterd.json`, and the
`skrouter/config` parser rejects them as well, so adaptor-only configuration
cannot live inside the router document. Add a second key to the router
configuration ConfigMap, alongside `skrouterd.json`/`skrouterd.json.gz`:

```json
// data["adaptor.json"]
{"version":1,"addressPrefixes":[{"prefix":"backend."}]}
```

Modelled as `qdr.AdaptorConfig` written by `kubeqdr.ConfigMapWriter` and read
by `kubeqdr.GetAdaptorConfigFromConfigMap`. It is small and is never
compressed. The controller derives it from `ExtendedBindings.perTargetListeners`
(one prefix per `exposePodsByName` Listener, `routingKey + "."`). Results are
hard-capped at `MaxPrefixMatches = 128` per prefix; the adaptor sets
`truncated` and the controller surfaces it as a Listener condition message.

### Transport

* Name: `<group>-status` (`skupper-router-status`, `skupper-router-2-status`).
* Labels: `internal.skupper.io/router-status: ""`,
  `internal.skupper.io/router-group: <group>`.
* Owner: the group's Deployment (as `skupper-network-status` is today).
* Payload: `binaryData["status.json.gz"]`, gzip, always compressed. Unlike
  ADR 0011 there are no pre-existing readers, so a single representation is
  simpler than a threshold. Inspect with
  `kubectl get cm skupper-router-status -o jsonpath='{.binaryData.status\.json\.gz}' | base64 -d | gunzip`.
* Size: O(local config). The document is bounded by the same things that
  bound `skrouterd.json`, plus ≤128 strings per `exposePodsByName` listener
  and one small entry per site in the network. If a status document ever
  exceeds 1MiB the API server rejects it and the adaptor logs; the plan does
  not add chunking (matches the decision for configuration).
* Publisher: the kube-adaptor for that group, under a **per-group** lease
  `<group>-status-leader`. The existing `skupper-site-leader` lease stays, but
  only for the kubeflow site controller (single SITE record emitter). Today
  the network status is site-wide and therefore single-leader; per-group
  documents need one publisher per group.
* Cadence: rebuild on (a) config sync completing, (b) vanflow SITE/ROUTER
  change, (c) a 10s poll of router management (link/listener oper state has
  no push channel over management). Publish only when the document differs
  from the last published one, rate limited to one write per second as
  `StatusSync` does today.

### Controller consumption

`Controller` watches ConfigMaps with `internal.skupper.io/router-status` and
routes to `Site.RouterStatusUpdated(group string, doc *status.Document)`.
`Site` keeps `map[group]*status.Document` and recomputes derived state on
every update. Merge rules across groups (HA):

* Link `Operational`: true if any group reports `connectionStatus == SUCCESS`.
  `remoteSiteId/Name` from the first group that has them. Message from the
  failing group when none succeed (new: users see the TLS/DNS error on the
  Link).
* Listener `Matched` (condition kept, meaning sharpened to "reachable"):
  true if any group reports its tcpListener `operStatus == up`. Message from
  `tcpListener.connectionMsg` when down with an error.
* MultiKeyListener `routingKeysReachable`: union across groups of `Addresses`
  with `reachable`, filtered to the MKL's keys, priority order preserved.
* `exposePodsByName`: `PerTargetListener.extractTargets` takes the union of
  `PrefixQuery.Matches` across groups for its prefix. Sets Configured with an
  error message if any group reports `truncated`.
* `Site.status.sitesInNetwork`: `max(len(doc.Network.Sites))` across groups.
* Configured-but-not-present: when a group's `Applied.ResourceVersion` is at
  or past the revision that introduced an entity and the entity is not
  `present`, surface `Applied.Error` on the owning CR's Configured condition.
  This is the generic "status for router configuration entities" (item 5) and
  replaces today's optimistic "Configured = written to ConfigMap".

### Deprecations

* `Site.status.network`: no longer populated. Field stays in `v2alpha1` with
  a `// Deprecated` comment and CRD description; removed in the next API
  version. `sitesInNetwork` stays.
* `Connector.status.hasMatchingListener`, Connector `Matched` condition,
  `AttachedConnectorBinding.status.hasMatchingListener`: no longer populated;
  Connector `Ready` = `Configured` only. Fields and constant stay, marked
  deprecated. `skupper connector status` drops the column.
* `skupper-network-status` ConfigMap: not created, not watched. `skupper debug
  dump` collects the `<group>-status` ConfigMaps (decompressed) instead.
* `internal/flow/status.go`, `internal/network` (`ExtractSiteRecords`,
  `GetLinkRecordsForSite`, `HasMatchingPair`, `SkupperStatus`), and
  `api/types/client.go: NetworkStatus()`: deleted once no caller remains.
* `Listener.status.hasMatchingConnector` stays; its value now comes from the
  router.

## Prerequisites

Both branches must land first; this plan builds on their APIs:

1. `origin/compressed-router-config` — `kubeqdr.ConfigMapWriter`,
   `GetRouterConfigFromConfigMap`, ADR 0011. The adaptor config key and the
   status writer reuse its gzip helpers (move `compressConfig`/
   `decompressConfig` to a shared `internal/kube/qdr/compress.go`).
2. `origin/add-skrouter-management-client` — `pkg/skrouter/mgmt`,
   `pkg/skrouter/mgmt/entities`, `pkg/skrouter/config`,
   `pkg/skrouter/reconcile`. The status builder queries through
   `mgmt.Query[entities.TcpListener]` etc.

The plan does not require migrating `ConfigSync` off `internal/qdr.AgentPool`
to `skrouter/reconcile`; the status builder opens its own `mgmt.Client` to
`amqp://localhost:5672`. Migrating config sync is a separate effort and can
happen before or after.

## Phases

Each phase is independently mergeable and leaves both status paths working
until Phase 5 removes the old one. Operators who cannot wait for Phase 5 can
turn the legacy path off as soon as Phase 2 ships with the cutover flag
described there. See "Release mapping" for the target releases.

### Phase 0 — ADR and schema

* Write `doc/adr/0012-local-first-router-status.md` from the Design section.
* Add `pkg/skrouter/status/document.go` with the types above and
  `document_test.go` covering JSON round trip and stable ordering (all slices
  sorted by name so `reflect.DeepEqual` is a valid change detector).
* Add `qdr.AdaptorConfig` (`internal/qdr/adaptor_config.go`) and the
  `adaptor.json` read/write in `internal/kube/qdr/configmap.go`. No producer
  yet.

Verify: `go test ./pkg/skrouter/status/... ./internal/kube/qdr/...`.

### Phase 1 — Adaptor publishes per-group status (additive)

`internal/kube/adaptor/status.go`:

* `StatusPublisher` struct: `mgmt.Client` (localhost:5672), a
  `status.Builder`, a `SiteIndex` fed by a reduced vanflow subscription, the
  ConfigMap client, group name, pod hostname.
* `status.Builder.Build(ctx, target mgmt.Target, desired *config.Document,
  adaptor qdr.AdaptorConfig, applied Applied, index SiteIndex) (Document,
  error)` in `pkg/skrouter/status/build.go`. Queries, in one batch per type:
  `router`, `connector` (role inter-router/edge), `connection` (dir out +
  role; dir in + role for RouterAccess peers), `listener` (role
  inter-router/edge), `tcpListener`, `tcpConnector`, `listenerAddress`,
  `router.address` (paged, `mgmt.Page`). For edge mode, prefix scans and
  address reachability for *non-referenced* addresses are issued to
  `client.Interior(uplinkID)`; uplink discovered from the open edge
  `connection.container`.
* `SiteIndex` (`pkg/skrouter/status/siteindex.go`): a `store.Interface`
  restricted to `vanflow.SiteRecord` and `vanflow.RouterRecord`, populated by
  the same `eventsource.Discovery` + `eventsource.Client` wiring as
  `StatusSync.handleDiscovery`, with `RecordStoreMap` containing only those
  two types so everything else is dropped at the router. Exposes
  `Sites() []Site` and `SiteForRouter(routerID) (Site, bool)`.
* Trigger wiring: `ConfigSync.configEvent` records
  `Applied{ResourceVersion, Error}` and calls `publisher.Notify()`;
  `SiteIndex` store handlers call `Notify()`; a 10s ticker calls `Notify()`.
  `Run` coalesces notifications, builds, compares with last published,
  writes via `retry.RetryOnConflict`.
* Leader election: `runLeaderElection` in `collector.go` gains a second lease
  `<group>-status-leader` whose `OnStartedLeading` runs the publisher. The
  existing `skupper-site-leader` callbacks keep `siteCollector` (old
  StatusSync) and `ensureStartFlowController` for now.
* `ConfigSync.configEvent` also reads `adaptor.json` and hands
  `qdr.AdaptorConfig` to the publisher.

Tests:

* `pkg/skrouter/status/build_test.go` against a fake `mgmt.Target` (the
  branch's `mgmt_test.go` shows the pattern) covering: listener up/down,
  connector FAILED with message, edge-mode prefix query routed to uplink,
  prefix truncation at 129 matches, address reachable on
  `remoteCount>0` with `subscriberCount==0`, ordering stability.
* `internal/kube/adaptor/status_test.go`: publish is skipped when unchanged;
  conflict retry; leader loss stops publishing.
* Integration: extend `tests/integration/kube/controller` suite (envtest +
  real router in `pkg/skrouter/mgmt/integration_test.go` style) to assert the
  ConfigMap appears, decompresses, and flips a tcpListener to `up` when a
  tcpConnector is added.

Verify: `go test ./pkg/skrouter/... ./internal/kube/adaptor/...`; cluster
smoke: `kubectl get cm skupper-router-status ... | gunzip | jq .`.

### Phase 2 — Controller publishes adaptor config and consumes status (dual-read)

* `Site.updateRouterConfigForGroup` / `createRouterConfigForGroup` also write
  `adaptor.json` from `ExtendedBindings.adaptorConfig()` (prefix per
  perTargetListener). `kubeqdr.UpdateRouterConfig` gains the adaptor config
  alongside `ConfigUpdate`.
* `Controller.routerStatusWatcher` = `WatchConfigMaps(label
  internal.skupper.io/router-status)`, handler `routerStatusUpdate` →
  `Site.RouterStatusUpdated(group, doc)`. Also processed during recovery next
  to `networkStatusWatcher.List()` in `Controller.init`.
* `Site` gets `routerStatus map[string]*status.Document` and a
  `deriveFromRouterStatus()` that implements the merge rules. In this phase
  it drives: Link Operational (+message), Listener Matched (+message),
  MultiKeyListener reachability, `exposePodsByName` targets,
  `sitesInNetwork`, and the Configured/present check. `NetworkStatusUpdated`
  stops touching those; it only continues writing `status.network` and
  Connector matched until Phase 3.
* `PerTargetListener.extractTargets(targets []string, ...)` takes a target
  list instead of `[]SiteRecord`; `findTargetsInNetwork` deleted.
* `binding_status.go`: keep `updateMatchingListenerCount*` (Connector side)
  on the old path for now; move Listener/MKL functions to take the merged
  status view.

**Cutover flag.** Some networks are already at the size where the legacy
flow is unsustainable, so this phase also ships a switch that turns the
legacy flow off entirely rather than waiting for Phase 5:

* Controller flag `-legacy-network-status` / env `LEGACY_NETWORK_STATUS`
  (`iflag.BoolVar` in `internal/kube/controller/config.go`, next to
  `REQUIRE_EXPLICIT_CONTROL`), default `true`. Operators set the env var on
  the controller Deployment; document it in `doc/` alongside the other
  controller env vars (the generated manifests and Helm chart need no change
  since the default is `true`).
* Controller with the flag off: does not create `networkStatusWatcher`, does
  not call `NetworkStatusUpdated`, never writes `Site.status.network`
  (cleared to nil on the next status write) or Connector
  `hasMatchingListener` (cleared to false); Connector Ready follows the
  Phase 3 rule (Configured only) immediately. All other status comes from
  `deriveFromRouterStatus()`.
* Controller plumbs the value into every router deployment group it renders
  (`skupper-router-deployment.yaml`, kube-adaptor container env
  `SKUPPER_LEGACY_NETWORK_STATUS`). Adaptor with the flag off: does not run
  `siteCollector`/`StatusSyncClient` or subscribe to whole-network vanflow;
  on becoming `skupper-site-leader` it deletes any `skupper-network-status`
  ConfigMap in its namespace. The lease still gates the kubeflow site
  controller. Because the flag rides on the router Deployment, flipping it
  rolls the routers once; document this.
* `skupper debug dump` stops listing `skupper-network-status` when the flag
  is off (the ConfigMap no longer exists).
* Phase 3 deprecation notices reference the flag as the way to opt out
  early. The flag is deleted in Phase 5 together with the code it guards; a
  controller started with `-legacy-network-status` after Phase 5 fails flag
  parsing, which is the intended signal to remove it from manifests.

Tests: rewrite `site_test.go` cases that feed `NetworkStatusUpdated` for
links/listeners/MKL/exposePodsByName to feed `RouterStatusUpdated` documents,
including an HA case where group 1 reports down and group 2 up, and a
truncated prefix result. `per_target_listener_test.go` against the new
signature. `controller_test.go`: status ConfigMap event routes to the right
site; malformed gzip is logged and ignored; with `LegacyNetworkStatus=false`
a `skupper-network-status` ConfigMap event is not delivered to the Site and
`Site.status.network` is nil after reconcile. Adaptor test: flag off → no
`StatusSync` started, leftover ConfigMap deleted on leader acquisition.

### Phase 3 — Deprecations in API and CLI

* `pkg/apis/skupper/v2alpha1/types.go`: deprecation comments on
  `SiteStatus.Network`, `SiteRecord`, `ServiceRecord`, `LinkRecord`,
  `ConnectorStatus.HasMatchingListener`,
  `AttachedConnectorBindingStatus.HasMatchingListener`. `Connector.
  SetConfigured` uses `setReady([]string{CONDITION_TYPE_CONFIGURED})`;
  `Connector.SetHasMatchingListener`/`setMatched` deleted (a stale `Matched`
  condition on existing Connectors is removed by `SetConfigured` on first
  reconcile: add a one-line `meta.RemoveStatusCondition`).
* Regenerate CRDs/docs (`make generate`, `doc/` CRD reference) with the
  deprecation text.
* `Site.NetworkStatusUpdated` stops writing `status.network`;
  `updateNetworkStatus` deleted. Clear an existing `status.network` once on
  upgrade so stale data does not linger (set to nil when non-nil).
* CLI: `skupper connector status` (kube and nonkube) drops the
  `HAS MATCHING LISTENER` column; `skupper listener status` keeps its column.
  `skupper link status` shows the new message on failure.
* `skupper debug dump`: replace `skupper-network-status` with the
  `<group>-status` ConfigMaps, writing decompressed `status.json` next to
  each.

Tests: CLI status command tests; `types_test.go` for Connector Ready without
Matched.

### Phase 4 — nonkube

nonkube has one router group and the system-controller runs on the same host.

* Replace `internal/nonkube/flow/collector.go` with a publisher that uses
  `pkg/skrouter/status` against the local router (`runtime.GetLocalRouterAddress`
  + `skupper-local-client` TLS, as today) and writes
  `runtime/ConfigMap-skupper-router-status.yaml` (plain JSON in `data` is fine
  on disk; no 1MiB limit, but keep the same key name for tooling parity).
* `network_status_handler.go` → `router_status_handler.go`; `SiteState.
  UpdateStatus(doc status.Document)` implements the same merge rules with a
  single group. Connector matched removed here too.
* Adaptor config for nonkube: `exposePodsByName` is Kubernetes-only, so no
  `adaptor.json` producer; the publisher accepts an empty prefix list.

Tests: `network_status_handler_test.go` rewritten for the new document;
`site_state_test.go` link/listener derivation.

### Phase 5 — Remove the old path

* kube-adaptor: delete `siteCollector`, `StatusSyncClient`, the
  `skupper-network-status` creation. `skupper-site-leader` lease now only
  gates the kubeflow site controller.
* Controller: delete `networkStatusWatcher`, `networkStatusUpdate`,
  `NetworkStatusUpdated`, `BindingStatus` Connector paths.
* Delete `internal/flow/status.go` (+test), `internal/network/*` except
  anything the network-observer still imports (check: it should import none),
  `api/types/client.go: NetworkStatus`.
* Delete the `-legacy-network-status` flag, `SKUPPER_LEGACY_NETWORK_STATUS`
  plumbing, and the branches it guarded; the flag-off behavior becomes the
  only behavior.
* Upgrade hygiene: the controller deletes a leftover `skupper-network-status`
  ConfigMap in controlled namespaces on startup (it is owned by the router
  Deployment, so it is also garbage-collected on the next router rollout;
  the explicit delete just makes the transition immediate).
* Update `doc/` and release notes: deprecations, new inspection command,
  new Link failure messages.

Verify: full `make test`, `go vet`, integration suite, and an HA cluster
smoke test (two groups, kill one router pod, observe Link Operational stays
true and `skupper-router-2-status` reflects the outage).

## Release mapping

| Release | Phases | User-visible result |
|---|---|---|
| 2.3 | 0, 1, 2 (incl. cutover flag), 3 | Both status paths run by default. Per-group `<group>-status` ConfigMaps and `adaptor.json` exist; Link/Listener/MKL/exposePodsByName status comes from router status. `Site.status.network` and Connector `hasMatchingListener` still populated but marked deprecated. `LEGACY_NETWORK_STATUS=false` removes the legacy flow entirely for operators at the size threshold. |
| 2.4 | 4, 5 | nonkube moves to the same document. Legacy flow, `skupper-network-status`, `Site.status.network`, Connector `hasMatchingListener`, and the cutover flag removed. |

The Site Capacity Contract document covers only the Kubernetes path; nonkube
(Phase 4) is this plan's addition and is what pins Phase 5 to 2.4 rather than
letting it ship earlier.

## Rollout and compatibility

* Controller and adaptor versions can skew for one release in either
  direction: Phase 1 adaptors publish both documents; Phase 2 controllers read
  both. Phase 5 ships one release after Phase 3 (2.4 after 2.3), and the
  cutover flag gives 2.3 operators the Phase 5 behavior early.
* Cutover flag skew: a 2.3 controller with `LEGACY_NETWORK_STATUS=false`
  and a not-yet-rolled 2.2 adaptor produces the same transient as a 2.4
  controller with an old adaptor (below); the flag reaches the adaptor only
  through the router Deployment the controller renders.
* A pre-Phase-2 controller ignores the new ConfigMaps (label not watched).
  A post-Phase-5 controller with an old adaptor sees no router status: Links
  stay `Operational: Pending`, Listeners `Matched: Pending` until the router
  Deployment rolls to the new adaptor image, which the controller does on
  upgrade anyway.
* Network observer is unaffected: it consumes vanflow directly and never read
  `skupper-network-status`.
* Console/UI consumers of `Site.status.network`, if any exist outside this
  repo, get one release of deprecation notice; `sitesInNetwork` remains.

## Open decisions (defaults chosen, veto welcome)

1. **Always gzip** the status document rather than reusing ADR 0011's
   threshold. Alternative: share `ConfigMapWriter` with a threshold of 0.
2. **Vanflow stays for SITE/ROUTER only**, giving Link remote site names and
   `sitesInNetwork`. Alternative: drop `remoteSiteName` and `sitesInNetwork`
   entirely and remove vanflow from the adaptor's status path; the router
   exposes `qd.access-id` and `container` but not the peer's site id.
3. **Per-group lease** for the publisher rather than making the status write
   idempotent across pods. During a rolling update two adaptor pods exist for
   ~seconds; a lease avoids the two pods alternately publishing different
   `Router.Hostname`/`Applied` values.
4. **Listener `Matched` is kept** (meaning: router reports the listener
   socket open because a destination is reachable). Alternative: rename to
   `Reachable`; kept to avoid churning the Listener API while Connector's
   side is deprecated.
5. **Prefix queries from edge routers go to the uplink interior's
   management address** rather than adding a router feature. This is how
   `qdr.Agent.GetInteriorNodes` already works from edge sites; a native
   prefix-watch in the router would be better long term and can replace this
   without changing the document.
6. **`adaptor.json` lives in the router config ConfigMap** rather than a
   third ConfigMap, so the adaptor's single watch and the controller's single
   write per group cover it.
7. **Poll interval 10s** for router management state. Link and listener oper
   state changes are not pushed over management; vanflow could be used as a
   trigger but it is the thing we are minimizing.
8. **Cutover flag is controller-wide**, not a `Site.spec.settings` key, and
   defaults to legacy-on in 2.3. Controller-wide because the cost being
   avoided is per network, not per site, and a flag that dies in one release
   should not enter the Site API. Alternative: default to legacy-off in 2.3
   and let the flag re-enable it; rejected because the deprecated fields
   would silently empty for everyone on upgrade.
