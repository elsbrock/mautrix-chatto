# 0006: Echo suppression and idempotency

Status: accepted (2026-10-25)

## Context
Chatto publishes `message_posted` on the WebSocket as soon as a message is
committed, and that can be before the `CreateMessage` response reaches the
bridge. bridgev2's own dedupe relies on the message ID being in its database,
which only happens after the response.

## Decision
- Every send carries `idempotency_key = UUIDv5(namespace, matrix_event_id)`.
  Retries of the same Matrix event within Chatto's 30-minute window return the
  original message instead of duplicating it.
- One `echoTracker` per bridge (not per login) counts in-flight sends per
  room, keyed by the server-prefixed portal ID. A `message_posted` whose author could be the bridge (any logged-in
  account or ghost bot) waits for the room's in-flight sends to finish, for up
  to 15 s. It is then dropped if one of those sends returned its ID. IDs are
  remembered for 10 minutes. Messages from other users never wait. Messages
  from a bridged account that were sent by its other Chatto clients are
  bridged normally (double puppeting).
- Edits: text parts store a hash of the bridged Chatto body. A
  `message_edited` echo of an edit made on Matrix, or an asset event, leaves
  the text unchanged and produces no Matrix edit.

## Consequences
A realtime event from our own account can be delayed by up to one send's
round trip. That only happens while the bridge itself is sending to that room.
