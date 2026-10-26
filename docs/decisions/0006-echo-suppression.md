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
- An `echoTracker` counts in-flight sends per room. A `message_posted` from the
  login's own account in that room waits (up to 15 s) for the in-flight sends
  to finish, then is dropped if its ID was returned by one of them. IDs are
  remembered for 10 minutes. Messages from the same account sent by other
  Chatto clients are bridged normally (double puppeting).

## Consequences
A realtime event from our own account can be delayed by up to one send's
round trip. That only happens while the bridge itself is sending to that room.
