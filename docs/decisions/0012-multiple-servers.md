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
- Each server is a space portal (`<id>.server`), named after the server's
  public profile. Channels and DMs are its children. With
  `network.group_spaces` (default off), room groups become subspaces between
  the server space and their channels.
- A login whose server is no longer configured stays loaded so it can be
  logged out, and reports `chatto-server-not-configured`.
  `strict_server_version` applies per server.

## Consequences
A single-server bridge still gets a server space, so adding a second server
later re-parents nothing. Renaming a server `id` orphans everything bridged
under it. Turning `group_spaces` off leaves the old group spaces behind.
