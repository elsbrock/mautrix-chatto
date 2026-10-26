# 0011: Read receipts, typing and presence

Status: accepted (2026-10-25)

## Decision
- **Read receipts:** Chatto exposes only the viewer's own read state. Matrix
  receipts mark the room (or thread) read on Chatto. Chatto
  `room_read_state_changed` events become read receipts for the user's own
  Matrix account, which needs double puppeting. Other users' read positions
  cannot be bridged.
- **Typing:** `RefreshTypingIndicator` is re-sent every 2 s while the Matrix
  user types, for at most 2 minutes. Chatto has no stop call; it expires
  indicators after 6 s, which is also the timeout used for ghosts.
- **Presence:** with `sync_presence`, Chatto presence and custom status are
  set on ghosts through the appservice intent. Online maps to online; away and
  DND map to unavailable (DND is added to the status message); anything else
  is offline. `keep_logins_online` refreshes the login's own presence every
  30 s, for setups where the bridge is the user's only Chatto client.
