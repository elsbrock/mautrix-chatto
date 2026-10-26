# 0007: Authentication flows

Status: accepted (2026-10-26)

## Decision
- **Password login:** `POST /auth/login` without the cookie header returns a
  bearer access token (15 min) and a single-use refresh token (90-day sliding
  window).
- **Token login** accepts a bot API key (`cht_BK_…`, never expires) or a
  refresh token (`cht_RT_…`). A refresh token is redeemed immediately, so the
  bridge takes over that session; the client it came from will be signed out
  on its next refresh.
- **Refresh safety:** refresh tokens are one-time-use, and presenting a stale
  one revokes the whole session. Refreshes are serialised per login. Before
  each refresh the session is saved with a `pending_refresh_id`, so a crash
  between the server rotating the token and the bridge saving it can be
  recovered by replaying the same request ID.
- **OAuth:** Chatto's OAuth needs a client metadata document (CIMD) hosted at
  an HTTPS URL that equals the `client_id`, with redirects to that host. The
  bridge can serve both from its public appservice address. This is
  implemented as an optional flow that is only offered when
  `appservice.public_address` is configured (see the README).
- Bot logins get no baseline permissions on Chatto. The
  `grant-bot-permissions` command lets the owner's human login grant the set
  the bridge needs, scoped to the server, a group or a room.
