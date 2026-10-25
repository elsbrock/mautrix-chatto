# TODO

Living, prioritised list. The top item is next.

## In progress
- [ ] pkg/chattoclient: HTTP auth (password login, refresh rotation, logout), connect
      clients with bearer interceptor and metrics, realtime WS (v4, resume cursors,
      heartbeat watchdog, backoff, close handling)

## Next
- [ ] pkg/connector: bridgev2 NetworkConnector, login flows (password, token, bot key)
- [ ] Portals: channels, DMs, group DMs, room groups → spaces, chat info and members
- [ ] Remote events: posted, edited, retracted, reactions, typing, presence, calls, room lifecycle
- [ ] Matrix events: message, edit, redaction, reaction, typing, read receipt, room name/topic
- [ ] Media: upload (chunked) and download (signed URLs), asset processing updates
- [ ] Backfill: FetchMessages via GetRoomEvents/GetThreadEvents, forward fill after downtime
- [ ] Echo suppression: idempotency keys from Matrix event IDs plus pending-send table
- [ ] Fake Chatto server (connect handlers + WS) and connector-level round-trip tests
- [ ] cmd/mautrix-chatto, example config, metrics, health endpoints
- [ ] flake.nix, NixOS module, CI, monitoring, docs, ADRs
- [ ] Relay mode and optional ghost bot accounts
- [ ] Bot permission setup command
- [ ] OAuth (CIMD) login flow

## Done
- [x] Vendor protos, generate connect-go (pkg/chattopb)
- [x] Emoji shortcode mapping (pkg/emoji)
- [x] Markdown ↔ Matrix HTML with Chatto mention/linkify parity (pkg/chattofmt)
