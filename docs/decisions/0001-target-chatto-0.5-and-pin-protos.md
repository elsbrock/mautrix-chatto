# 0001: Target Chatto 0.5 and pin its protos

Status: accepted (2026-10-25)

## Context
Chatto's stable docs (docs.chatto.run) describe the 0.4 line with realtime
protocol v1 (`hello` frames). The newest tag, `v0.5.0-beta.21`, speaks
realtime **v4**: a single `RealtimeSubscribe` frame, resume cursors, snapshots
and typed close frames. The two are not compatible.

## Decision
- Build against `v0.5.0-beta.21` and vendor its `proto/` into `proto/`.
  The generated Go code is committed in `pkg/chattopb`.
- Keep the upstream version in one place: `chattoclient.UpstreamVersion`.
  `scripts/sync-proto.sh` reads it, and so do metrics and `chatto-server`.
- Check the server version at startup with semver. An older server is logged,
  or refused when `network.strict_server_version` is true. Non-semver versions
  (dev builds) are accepted.
- `proto/chatto/realtime/v1` declares no service, only the frames sent over
  the `/api/realtime` WebSocket, so buf generates message types for it and no
  connect-go client. `pkg/chattoclient/realtime.go` is the realtime client,
  and `pkg/chattotest/realtime.go` is its scriptable fake. The operator
  package does have a service and gets a client too, though the bridge does
  not call it.

## Consequences
0.4 servers are not supported. Upgrading to a newer Chatto means running
`scripts/sync-proto.sh <tag>`, fixing compile errors and re-running the tests.
The vendored upstream mention test cases keep formatting parity checked.
