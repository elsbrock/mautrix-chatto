# 0003: Go and mautrix bridgev2, one Chatto server per bridge

Status: accepted (2026-10-25)

## Decision
- Written in Go on mautrix-go `bridgev2` (v0.32), following mautrix-signal's
  layout (`cmd/`, `pkg/connector`). bridgev2 handles the appservice, portals,
  ghosts, backfill queue, double puppeting, relay mode, encryption and
  provisioning.
- One bridge instance serves exactly one Chatto server
  (`network.server_url`). Chatto IDs are only unique per server, and a
  homelab typically runs one server. Supporting several servers would mean
  prefixing every ID, which is not worth it.
- Builds use the `goolm` tag (pure-Go olm), so libolm is not needed.

## Consequences
Multiple accounts on the same server are supported (several logins per Matrix
user, and many users). Bridging a second Chatto server takes a second bridge
instance with its own appservice ID.
