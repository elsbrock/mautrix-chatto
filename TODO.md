# TODO

Living, prioritised list. The top item is next.

## Next
- [ ] Try against a real Chatto 0.5 server and homeserver
- [ ] Pins from Matrix to Chatto (needs a bridgev2 hook for m.room.pinned_events)
- [ ] Map Chatto roles to Matrix power levels (read-only)
- [ ] Bridge Matrix room invites of ghosts into DMs for bot logins (currently Chatto forbids bot DMs)
- [ ] Batch member and backfill-need checks after a snapshot on large servers
      (currently one ListMembers and one GetRoomEvents per room)
- [ ] Direct media (serve Chatto signed URLs through the bridge instead of reuploading)
- [ ] Image dimensions and thumbnails for Matrix → Chatto uploads
- [ ] goreleaser config and published release binaries

## Done
- [x] Vendor protos, generate connect-go (pkg/chattopb), sync script
- [x] Emoji shortcode mapping (pkg/emoji)
- [x] Markdown ↔ Matrix HTML with Chatto mention/linkify parity (pkg/chattofmt)
- [x] Client: password/refresh/logout/OAuth, interceptor with retry and metrics
- [x] Realtime v4: cursors, snapshots, heartbeat watchdog, backoff, close handling
- [x] Connector: logins (password, token, bot key, OAuth), portals, spaces, DMs
- [x] Messages, edits, deletes, reactions, replies, threads, media, typing,
      receipts, presence, room name/topic, membership, calls, invitations
- [x] Backfill and forward fill, echo suppression, idempotency keys
- [x] Relay mode and ghost bot accounts
- [x] Metrics, health endpoints, Grafana dashboard, alert rules
- [x] Fake Chatto server, mock homeserver, connector round-trip tests, fuzzing
- [x] flake.nix: package, overlay, devshell, NixOS module, checks
- [x] CI, docs, ADRs
