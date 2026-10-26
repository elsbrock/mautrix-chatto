# Good morning ☀️

**mautrix-chatto** is a Matrix ↔ Chatto puppeting bridge in Go on
mautrix-go bridgev2. It targets Chatto `v0.5.0-beta.21` (realtime protocol
v4) and comes with a Nix flake, a NixOS module, Prometheus metrics, a Grafana
dashboard and alerts.

## What works (verified with synthetic tests)

All of this runs against `pkg/chattotest`, an in-memory Chatto built on the
real generated API handlers, and a mock homeserver driving a real bridgev2
bridge on SQLite:

- **Logins:**
  - password, browser OAuth (PKCE with a hosted client metadata document),
    bot API key, or refresh-token takeover
  - several logins per user
  - refresh tokens are single-use and rotated crash-safely
- **Portals:**
  - channels, DMs and group DMs
  - room groups as spaces
  - name and topic both ways
  - membership, archive and delete handling
- **Messages, both ways:**
  - Markdown ↔ HTML using Chatto's own mention rules (its 86 shared test
    cases pass)
  - edits, deletes, reactions (emoji ↔ gemoji shortcodes), replies, threads
  - attachments (chunked upload; placeholders for videos still processing,
    replaced when ready)
- **Ephemeral:** typing, your own read state, presence and custom status;
  call start and end notices.
- **Realtime:**
  - resume cursors, snapshot fallback, forward fill after a gap
  - heartbeat watchdog, jittered backoff honouring `retry_after`
  - token refresh on `AUTHENTICATION_REQUIRED`
- **Duplicates:**
  - echo suppression, including the realtime-before-RPC-response race and
    across multiple logins
  - idempotency keys derived from Matrix event IDs
- **Backfill:** initial history, threads and reactions.
- **Relay mode**, plus optional **ghost bot accounts**: one Chatto bot per
  Matrix user without a login, so Chatto shows real senders.
- **Bot logins**, with a `grant-bot-permissions` command, since Chatto bots
  start with no permissions.
- **Ops:**
  - `/metrics`, `/healthz`, `/readyz`
  - Grafana dashboard and alert rules (promtool-checked)
- **Checks:**
  - `nix build` and `nix flake check` pass on x86_64-linux (build, tests,
    lint, nixfmt, alert rules, NixOS module)
  - `go test -race` and golangci-lint are clean

## How it was checked

Besides the tests, two independent code reviews ran overnight against the
Chatto and mautrix sources. The first found 11 issues, all fixed with
regression tests:
- a refresh outage was reported as "logged out"
- forward fill broke when the newest message was a thread reply
- an event was dropped when a fetch failed transiently
- an edit could turn captioned media into text
- no-op edits, receipts on notices, lock contention, a metadata race, and
  URL tickets in logs

The second, on the NixOS module, config, security and formatting, found 11
more, also fixed:
- Chatto would have rejected the OAuth metadata document (cross-origin
  `client_uri`)
- an OAuth link could be forwarded to someone else; there's now a
  confirmation page naming the Matrix account
- the Synapse address and first-boot ordering were wrong
- Synapse could read the bridge database
- `2*3*4` rendered as italics
- ghost bots broke across different relay logins

GitHub Actions runs lint, race tests, fuzzing, a generated-code drift check,
`nix flake check` and `nix build`.

## Try it on your NixOS homelab (≈10 minutes)

1. **Add the flake input and module** (full snippet in the README):
   ```nix
   inputs.mautrix-chatto.url = "github:elsbrock/matrix-chatto-bridge";
   # modules = [ mautrix-chatto.nixosModules.default ];
   services.mautrix-chatto = {
     enable = true;
     settings = {
       network.servers = [ { id = "main"; url = "https://chat.example.com"; } ];   # your Chatto; id never changes
       homeserver = { address = "http://127.0.0.1:8008"; domain = "example.com"; };
       bridge.permissions = { "example.com" = "user"; "@you:example.com" = "admin"; };
     };
     metrics.enable = true;
   };
   ```
2. **Rebuild:** `nixos-rebuild switch`.
   - With Synapse on the same box, the registration is wired up automatically.
   - With tuwunel or conduwuit, register
     `/var/lib/mautrix-chatto/chatto-registration.yaml` through the admin room
     (`!admin appservices register` plus the YAML).
3. **Log in:** DM `@chattobot:example.com`, send `login`, pick **Password** and
   enter your Chatto credentials. Your rooms should appear within seconds.
4. **Watch it:**
   - `curl 127.0.0.1:8001/metrics | grep mautrix_chatto_realtime_connected`
     should show 1.
   - Import `deploy/monitoring/grafana-dashboard.json`.
   - `journalctl -u mautrix-chatto -f` shows the logs.

Optional: put the bridge behind HTTPS (`settings.appservice.public_address`)
to get the **Browser** login. For a bot, log in with its `cht_BK_…` key, then
run `grant-bot-permissions <bot>` from your human login inside a portal.

## Not yet verified against real servers

Everything above is tested against fakes written from Chatto's source, not
against a running Chatto. Most worth checking first:

1. **Realtime:** does the WebSocket subscribe and stay connected?
   `mautrix_chatto_realtime_connected` should be 1, and a message should
   arrive within a second.
2. **Password login:** Chatto may require a CSRF or Origin header I didn't
   find; the server code says no.
3. **Signed asset URLs and S3 redirects** for attachments, and chunk size and
   max size from `GetRuntimeConfig`.
4. **OAuth:** Chatto has to be able to fetch the metadata document from your
   public address, and the consent screen has to redirect back.
5. **Bot permissions** via `SetUserPermission`: grants are capped by your own
   permissions and may need privileged mode for some.
6. **Ghost bots:** `CreateBot` needs `bot.create`, which only admins have by
   default.
7. **Threading modes:** in REQUIRED rooms a Matrix reply to a root goes into
   the root's thread; in DISABLED rooms thread messages become replies.

## Known gaps

- **Pins:** only Chatto → Matrix (bridgev2 doesn't pass Matrix pin changes to connectors).
- **Power levels:** Chatto roles are not mapped to Matrix.
- **Read receipts:** only your own Chatto read state is visible (Chatto
  doesn't expose others').
- **Spoilers and underline:** Chatto has neither, so text is sent plain.
- **Escaping:** literal Markdown in Matrix plain text can't be escaped,
  because Chatto disables escapes.
- **Group DMs:** new ones can't be created (Chatto forbids it); existing ones
  bridge fine.
- **Bot logins:** can't start DMs (a Chatto rule).
- **Calls:** only start and end notices.
- **Large servers:** after a snapshot recovery every member room is resynced
  with a member listing and a timeline check. That is fine for a homelab but
  should be batched for big servers.

## Next steps

1. Run it against your Chatto and fix whatever the real server disagrees with
   (start with the realtime handshake and attachments).
2. Role → power level mapping.
3. Container image and goreleaser for non-Nix users.
4. Upstream: ask Chatto for an appservice-style API (posting as other users,
   or a per-message display name). It would make ghost bots unnecessary.

Design decisions are in `docs/decisions/` (11 ADRs). Protocol facts the bridge
relies on are in `docs/chatto-protocol-notes.md`. The living backlog is in
`TODO.md`.
