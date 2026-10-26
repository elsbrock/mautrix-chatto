# 0012: Several Chatto servers per bridge, one space per server

Status: accepted (2026-10-26). Supersedes 0003's one-server rule.

## Context
0003 limited a bridge to one Chatto server because Chatto IDs are only unique
per server, and prefixing every ID did not seem worth it. A Chatto server is a
workspace, and mautrix-slack and mautrix-discord bridge many workspaces from
one instance with one space each. The bridge has no deployments yet, so the ID
scheme can still change for free. Changing it later would mean migrating
every portal, ghost, login, message and ghost bot record.

## Decision
- `network.servers` lists the servers, each with a short `id`
  (`[a-z0-9-]{1,16}`) and a `url`. The `id` is permanent: it is part of every
  network ID and therefore of every ghost MXID. Only `url` may change, so a
  server can move without losing its rooms.
- Every server-scoped network ID is `<id>.<chatto id>`: ghosts, logins,
  portals and messages. `.` is used because ghost IDs become MXID localparts,
  where `:` would be escaped. Server IDs and Chatto IDs never contain `.`.
- A login belongs to the server in its ID. A login never acts on a portal or
  ghost of another server. bridgev2 can pick such a login (relay, default
  login), so the connector refuses those requests.
- A login belongs to one server, so its personal filtering space is that
  server's space. The connector names it after the server's public profile
  and sets its topic and logo. Room groups are always space portals
  (`<id>.group:<G…>`) without a parent, so bridgev2 adds them to the personal
  space. Channels sit in their group. Ungrouped channels and DMs have no
  parent and sit directly in the personal space. This mirrors Chatto's own
  sidebar: server → groups → channels.
- A login whose server is no longer configured stays loaded so it can be
  logged out, and reports `chatto-server-not-configured`.
  `strict_server_version` applies per server.

## Consequences
Adding a second server later re-parents nothing, because each login already
has its own space. Renaming a server `id` orphans everything bridged under it.
The server space depends on `bridge.personal_filtering_spaces` (on by
default). Without it, group spaces, ungrouped channels and DMs are top-level.
Group spaces are shared between the users of a server. Each user's DMs stay
in their own personal space. A group gets a space once the login is in one
of its channels, so groups without such a channel stay hidden. Group spaces
are refreshed on connect and on Chatto layout changes. bridgev2 does not
re-parent cleanly in two cases: a channel that leaves all groups stays
listed in its old group space, and an ungrouped channel that joins a group
stays in the personal space as well. A deleted group leaves its space behind.
