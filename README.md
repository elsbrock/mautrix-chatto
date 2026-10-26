# mautrix-chatto

A Matrix ↔ [Chatto](https://chatto.run) puppeting bridge, built on
[mautrix-go](https://github.com/mautrix/go) bridgev2.

It connects one or more Chatto servers (0.5 line, realtime protocol v4) to
your Matrix homeserver:
- Each Chatto server becomes a Matrix space with the server's name and logo.
- Room groups become subspaces, as in Chatto's sidebar.
- Chatto channels, DMs and group DMs become Matrix rooms in their group, or
  directly in the server space.
- Messages, edits, deletions, reactions, replies, threads, attachments,
  typing, read state and presence flow both ways.

> **Status:** feature-complete against Chatto `v0.5.0-beta.21`. It is tested
> against an in-memory fake of Chatto's API and a mock homeserver, but has not
> been run against a real Chatto server yet.

## Features

| Feature | Chatto → Matrix | Matrix → Chatto | Notes |
|---|:-:|:-:|---|
| Text with formatting | ✅ | ✅ | Chatto's Markdown dialect ↔ Matrix HTML. Bold, italic, strike, code, code blocks, quotes, lists, headings, tables, links, footnotes |
| Mentions | ✅ | ✅ | `@login` ↔ pills; `@all`/`@here` → `@room`. Uses Chatto's own mention rules |
| Edits | ✅ | ✅ | Chatto authors can edit for 3 h |
| Deletes | ✅ | ✅ | Deleting one attachment of a multi-attachment message removes just that attachment |
| Reactions | ✅ | ✅ | Unicode ↔ gemoji shortcodes; skin tones are folded (Chatto has none). Custom emoji are rejected |
| Replies | ✅ | ✅ | |
| Threads | ✅ | ✅ | Respects the room's threading mode; in rooms without threads a thread message becomes a reply |
| Attachments | ✅ | ✅ | Chunked upload to Chatto; signed URLs downloaded; videos that are still processing are replaced when ready |
| Captions | ✅ | ✅ | A Chatto message with text and attachments becomes separate Matrix events (ADR 0004) |
| Link previews | ✅ | ➖ | Chatto previews become Beeper link previews; Chatto generates its own previews for messages from Matrix |
| Stickers, voice, GIFs | ➖ | ✅ | Sent as attachments |
| Locations | ➖ | 🟡 | Sent as text with a `geo:` link |
| Typing | ✅ | ✅ | Refreshed every 2 s; thread typing shows as room typing |
| Read receipts | 🟡 | ✅ | Only your own read state exists on Chatto (needs double puppeting) |
| Presence and custom status | ✅ | 🟡 | Ghost presence plus status message; optionally keeps logins online |
| Room name and topic | ✅ | ✅ | Channels only |
| Room avatars | ➖ | ➖ | Chatto rooms have no avatars |
| User avatars and names | ✅ | ➖ | |
| Servers and room groups → spaces | ✅ | ➖ | One space per server, room groups as subspaces |
| Membership | ✅ | 🟡 | Leaving, adding (invite) and removing (kick) Chatto users in channels |
| DMs | ✅ | ✅ | Start a DM with `start-chat @login` (human accounts only; Chatto forbids bot DMs) |
| Group DMs | ✅ | ❌ | Chatto no longer lets anyone create new group DMs |
| Archived and deleted rooms | ✅ | ➖ | Archived rooms get a notice and topic marker, and sends are refused |
| Voice calls | 🟡 | ❌ | Start and end notices; no media bridging |
| Pins | ✅ | ❌ | Chatto pins set `m.room.pinned_events`; bridgev2 has no hook for pins made on Matrix |
| Room invitations | 🟡 | ➖ | Human accounts get a notice with the `accept-invite <invitation ID>` command to run |
| Backfill | ✅ | ➖ | Initial history, threads, and forward fill after downtime |
| Double puppeting | ✅ | ✅ | bridgev2 built-in |
| Relay mode | ✅ | ✅ | bridgev2 relay, or optional per-user **ghost bot** accounts on Chatto |
| Encryption | ✅ | ✅ | bridgev2 built-in (pure-Go olm) |

✅ supported · 🟡 partial · ➖ not applicable · ❌ not supported (see the notes)

## Logging in

Send `login` to the bridge bot. With more than one server configured, every
flow first asks which server, by its `id` from `network.servers` (for example
`login password work alice hunter2`). The available flows are:
- **Password:** Chatto username or email plus password. The server can turn
  this off (`auth.direct_login`), and so can the bridge
  (`network.allow_password_login`).
- **Browser (OAuth):** only offered when `appservice.public_address` is a
  public `https://` URL. The bridge hosts its OAuth client metadata document
  at `/chatto/oauth/client.json`. Chatto has no client registration.
  Chatto fetches that document itself and refuses private, loopback and CGNAT
  addresses (including Tailscale's 100.64.0.0/10), so the public address must
  resolve to a public IP. Before linking, the callback page shows which Matrix
  account will receive the session.
- **Token:** a bot API key (`cht_BK_…`) or a session refresh token
  (`cht_RT_…`). The bridge takes over a refresh token, so the session it came
  from will be signed out.

Each Matrix user can have several Chatto logins, also on different servers.
A login only ever acts in rooms of its own server; with several servers,
`list-logins` shows the server after each login's name.

### Several servers

Add another entry to `network.servers` and restart. Existing logins and rooms
are untouched, and the appservice registration doesn't change. Each server
appears as its own space, named after the server.

The `id` is permanent. It is part of every ID the bridge stores and of every
ghost's Matrix ID (`@chatto_<id>.<Chatto user ID>:example.com` with the default
`username_template`), so keep it short. It must be 1-16 characters of `a-z`,
`0-9` and `-`. Changing it later orphans everything bridged under it; only
`url` may change. See [ADR 0012](docs/decisions/0012-multiple-servers.md).

Each server's space is the Matrix user's personal space for that login, so
it needs `personal_filtering_spaces` (on by default). With it off, room
groups, ungrouped channels and DMs show up as top-level rooms.

Commands that act on Chatto (`accept-invite`, `grant-bot-permissions`) use the
server of the room they're run in. Elsewhere, add `--server <id>` when you are
logged into more than one server.

### Bot accounts

Chatto bots start with **no permissions**, can't be invited, and can't start
DMs. To bridge a bot account:

1. Log in with the bot's API key.
2. Log in with the human account that owns the bot.
3. Run `grant-bot-permissions <bot>` inside a portal to grant the room
   scope and add the bot to that room, or with `server` or `group <group ID>`
   for a wider scope. `<bot>` is the bot's Chatto username or user ID. The
   human login and the bot must be on the same server; the server comes from
   the portal, from the bot's own login, or from `--server <id>`.

It grants `room.list`, `room.join`, `message.read`, `message.post`,
`message.post-in-thread`, `message.echo`, `message.attach` and
`message.react`. Chatto caps grants at the owner's own permissions.

### Matrix users without a Chatto account

- **Relay mode:** run `set-relay` in a portal. Messages from users without a
  login are then posted by the relay login with a name prefix.
- **Ghost bots:** with `network.ghost_bots.enabled`, the relay login (which
  must be a human account with `bot.create`) creates a Chatto bot per Matrix
  user. Messages then appear under that user's own name, marked as a bot, with
  no prefix. See [ADR 0005](docs/decisions/0005-matrix-users-without-chatto-accounts.md).

## Quickstart: NixOS

```nix
{
  inputs.mautrix-chatto.url = "github:elsbrock/mautrix-chatto";

  outputs = { nixpkgs, mautrix-chatto, ... }: {
    nixosConfigurations.homelab = nixpkgs.lib.nixosSystem {
      modules = [
        mautrix-chatto.nixosModules.default
        {
          services.mautrix-chatto = {
            enable = true;
            settings = {
              # id is permanent: it is part of every bridged ID, including
              # ghost Matrix IDs. Only url may change later.
              network.servers = [ { id = "main"; url = "https://chat.example.com"; } ];
              homeserver = {
                address = "http://127.0.0.1:8008";
                domain = "example.com";
              };
              bridge.permissions = {
                "example.com" = "user";
                "@you:example.com" = "admin";
              };
            };
            metrics.enable = true; # /metrics, /healthz, /readyz on 127.0.0.1:8001
            # environmentFile = "/run/secrets/mautrix-chatto.env";
          };
        }
      ];
    };
  };
}
```

- **Synapse:** with `services.matrix-synapse` on the same host, the
  registration file is added automatically (`registerToSynapse`).
- **Other homeservers:** point them at `/var/lib/mautrix-chatto/chatto-registration.yaml`.
- **Prometheus:** with `services.prometheus` enabled, a scrape job and the
  alert rules are added for you.

## Quickstart: any Linux host

```sh
nix build github:elsbrock/mautrix-chatto   # or: go build -tags goolm ./cmd/mautrix-chatto
./result/bin/mautrix-chatto -e -c config.yaml    # writes the example config
$EDITOR config.yaml                              # network.servers, homeserver.*, bridge.permissions
./result/bin/mautrix-chatto -g -c config.yaml -r registration.yaml
./result/bin/mautrix-chatto -c config.yaml -r registration.yaml
```

Register `registration.yaml` with your homeserver:

- **Synapse:** add it to `app_service_config_files` and restart.
- **tuwunel / conduwuit:** send `!admin appservices register` in the admin
  room, followed by the YAML in a code block.

[example-config.yaml](example-config.yaml) is the complete annotated config.
The Chatto-specific part is the `network:` section.

## Container image

```sh
nix build github:elsbrock/mautrix-chatto#docker && docker load < result
docker run --rm -v $PWD/data:/data mautrix-chatto -e -c /data/config.yaml   # write a config
# edit data/config.yaml: set appservice.hostname to 0.0.0.0, then
docker run --rm -v $PWD/data:/data mautrix-chatto -g -c /data/config.yaml -r /data/registration.yaml
docker run -d -v $PWD/data:/data -p 29340:29340 mautrix-chatto
```

## Monitoring

- Enable `network.metrics.enabled` to serve these on `network.metrics.listen`:
  - `/metrics` (Prometheus)
  - `/healthz` (process alive)
  - `/readyz` (connector started)
- The metrics cover:
  - realtime connection state, reconnects, recoveries and event lag
  - RPC latency and errors per method
  - messages bridged and failures by direction and type
  - token refreshes
  - logins by state
  - the Chatto server version
- [`deploy/monitoring/grafana-dashboard.json`](deploy/monitoring/grafana-dashboard.json)
  is a ready-made dashboard. The NixOS module provisions it when
  `services.grafana` is enabled (`services.mautrix-chatto.grafanaDashboard`).
- [`deploy/monitoring/alerts.yaml`](deploy/monitoring/alerts.yaml) has
  Prometheus alert rules for a dead endpoint, disconnected logins, logins
  needing re-auth, reconnect storms, event lag, bridging errors, RPC error
  rate and an outdated server.

## Development

```sh
nix develop          # go, gopls, golangci-lint, buf, protoc plugins
scripts/check.sh     # vet, lint, race tests: run before pushing
nix flake check      # build, tests, lint, nixfmt, alert rules, NixOS module
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the layout, the test harness and how
to update to a new Chatto release. The design decisions are in
[`docs/decisions`](docs/decisions); protocol details in
[`docs/chatto-protocol-notes.md`](docs/chatto-protocol-notes.md).

## License

AGPL-3.0-or-later. The vendored Chatto protos under `proto/` are Apache-2.0
(see [ADR 0002](docs/decisions/0002-proto-licensing.md)).
