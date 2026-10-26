# 0004: Identifier and portal mapping

Status: accepted (2026-10-25)

## Decision
- **Server prefix:** ghost, login, portal and message IDs are prefixed with
  the server `id` and a dot (0012). The examples omit it. Part and emoji IDs
  live under a message and stay unprefixed.
- **Ghosts:** a ghost ID is the Chatto user ID (`U…`). A login ID is also the
  Chatto user ID, so two Matrix users can never hold the same Chatto account.
- **Portals:** a portal ID is the room ID (`R…`). Receivers are never split:
  room IDs are global on the server, and a DM is one room shared by its
  participants.
- **Spaces:** room groups are space portals with the ID `group:<G…>` and no
  parent. Channels in a group set `ParentID` to it. Ungrouped channels and DMs
  have no parent. The server itself is the login's personal filtering space,
  not a portal (0012).
- **Messages:** a message ID is the event ID (`E…`). The realtime event ID
  equals the message ID for `message_posted`. The text body is part `""`, and
  each attachment is part `att:<attachment id>`. Bridge notices use
  `notice:<server>.<room>:<nanos>`.
- **Captions:** a Chatto message with text and attachments becomes separate
  Matrix events rather than a captioned media event. An edit then changes
  exactly one Matrix event, and attachments that finish processing replace
  their own placeholder. Matrix captions (`body` ≠ `filename`) are sent as
  the Chatto message body.
- **Reactions:** the emoji ID is Chatto's gemoji shortcode. Matrix keys are
  normalised (VS16 removed, skin tone dropped, `:name:` accepted).
- **Threads:** Chatto threads map to `m.thread` on the root. A thread reply
  whose `in_reply_to` is the root is a plain thread message; other
  `in_reply_to` values become Matrix replies. Channel echoes of thread replies
  (`also_send_to_channel`) are skipped by default; `thread_echo: notice`
  posts a short notice instead.

## Consequences
Chatto pins, slow mode and room universality have no Matrix representation.
They are stored in portal metadata where useful (threading mode, universal,
archived) and otherwise ignored.
