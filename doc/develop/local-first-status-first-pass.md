# Local-first status: Kubernetes first pass

This implements the local management → per-group gzip ConfigMap → controller
status path from `local-first-status-plan.md`. It is a first pass, not completion
of all migration phases. This branch builds on `origin/main` and uses the
existing `internal/qdr` configuration parser and management agent. The status
document, builder, and network index live in `internal/routerstatus`; no new
router client library is required.

## Enable and inspect

Set `LEGACY_NETWORK_STATUS=false` on the controller (or pass
`-legacy-network-status=false`). The default is true. The controller renders
`SKUPPER_LEGACY_NETWORK_STATUS=false` into every router group's adaptor; changing
the setting rolls the router deployments.

With legacy disabled, the adaptor does not start the legacy StatusSync collector,
the site leader removes `skupper-network-status`, and the controller does not
watch it. Site `status.network` and Connector matching status are cleared;
Connector Ready depends on Configured. Listener Matched means the router reports
a reachable destination, not that the backend application is healthy.

Each group publishes `<group>-status`, owned by its Deployment, under a separate
`<group>-status-leader` lease. Management is polled every ten seconds, with
additional notifications after config sync and SITE/ROUTER changes. Identical
documents do not cause ConfigMap writes. `skupper debug dump` includes raw and
decompressed group status.

```sh
kubectl -n east get cm skupper-router-status \
  -o jsonpath='{.binaryData.status\.json\.gz}' | base64 -d | gunzip | jq .
kubectl get sites,links,listeners,connectors -A
```

## Original implementation's live orb demo

The following records the original feature branch's smoke test, before the
port to `internal/qdr`. It is not evidence of a live test of this port, and the
temporary cluster and files may no longer exist.

The isolated cluster was `local-first`, context `kind-local-first`. It contained
`west` and `east` sites, a Link from east to west, a `backend` Listener on west
port 8080, and a Connector on east targeting nginx pods on port 80. Both routers
and the controller use locally built images, not published images. The CLI is
`/tmp/local-first/skupper`; setup manifests and build binaries are in
`/tmp/local-first/`. Use the orb's Terminal tab:

```sh
kubectl --context kind-local-first get sites,links,listeners,connectors -A
/tmp/local-first/skupper link status --context kind-local-first -n east
/tmp/local-first/skupper listener status --context kind-local-first -n west
```

Manually exercised:

1. Unlinked sites each report one site. Listener without a connector is Pending;
   its management document reports `operStatus: down`.
2. Link creation reports Operational and remote site west. Both sites converge
   to `sitesInNetwork: 2`, with no `status.network` or legacy ConfigMap.
3. Adding the remote Connector makes Listener Matched/Ready and management
   `operStatus: up`. HTTP through west's listener returns nginx's welcome page.
4. Deleting the Connector makes the Listener Pending/down; recreating it recovers.
5. Replacing the Link endpoint port with a closed port makes Operational false
   and exposes the router's Connection refused error. The Listener also goes
   down. Restoring the endpoint recovers both.
6. Controller and router rollouts recover the status documents and CR statuses.

The first traffic attempt used the Service port rather than nginx's pod port;
it failed and was corrected to port 80 before the successful traffic check.

## Corrections to the plan and remaining work

- Management `connection.name` is `connection/<host:port>`, not the configured
  connector name. Peer association uses destination endpoint, direction, and
  role. Ambiguous peers remain unknown. Incoming peers require `localSocket`,
  which older router images may omit. Failover/proxy endpoint correlation needs
  additional coverage.
- Current router management can omit `router.version`; the retained ROUTER
  record's build version is the fallback.
- `RecordStoreMap` restricts retained data, not the router's transmitted stream.
  The current eventsource client has no server-side record-type filter. The
  index discards non-SITE/ROUTER records, but cannot eliminate their wire cost.
- Kubernetes resource versions are opaque. The document exposes the attempted
  revision, sync error, and per-entity presence, but this pass does not infer
  Configured errors by ordering resource versions.
- HA merge is covered by unit tests. Hard-outage expiry of last-known group
  documents is not implemented; a dead publisher's last document can remain
  until a replacement publishes or the ConfigMap is removed. This is not a
  production liveness guarantee.
- Multi-key and prefix paths are implemented, but the live smoke test targets
  Link, Site, and ordinary TCP Listener behavior. Edge prefix routing, prefix
  truncation/recovery, and a multi-group outage need dedicated live coverage.
- Adaptor prefix configuration currently uses a second, change-suppressed
  ConfigMap update after the router configuration update.
- Nonkube migration, API/CLI deprecation rollout, final legacy removal, and the
  standalone ADR remain future stages. Legacy-enabled controllers retain the
  old status fallback until local documents arrive.

Verification commands include `go test -short ./...`, targeted
router-status/controller/site tests, publisher conflict/no-op/cancellation tests,
and race checks of the publisher and network index. See the accompanying work
report for the checks actually executed for this port.
