# 0005: Matrix users without a Chatto account

Status: accepted (2026-10-26)

## Context
Chatto has no per-message display name override or masquerade. No admin API
creates human accounts. The Operator API can (and can import messages as
anyone), but it is a local unauthenticated unix socket, and its message import
is explicitly historical: no live delivery and no notifications. Bot accounts
can be created by any human with `bot.create`, and their owners can rename
them, add them to rooms and grant them permissions.

## Decision
1. **Puppeting** is the primary mode: each Matrix user logs in with their own
   Chatto account (password, refresh token or bot API key).
2. **Relay mode** is bridgev2's built-in relay. A portal's relay login posts
   messages from users without a login, prefixed with their name.
3. **Ghost bots** (optional, `network.ghost_bots.enabled`): for relayed
   senders the relay login creates a dedicated Chatto bot account per Matrix
   user. It adds the bot to the room, grants the room-scoped bot permissions,
   and posts as that bot, so Chatto shows the sender as a distinct account
   (marked as a bot) instead of a name prefix.
4. The Operator API is not used.

## Consequences
Ghost bots need a human relay login with `bot.create`. Each bot counts
against Chatto's bot limits. Bots created by the bridge are hidden from
contact lists and member syncs, so they don't appear twice on the Matrix side.
