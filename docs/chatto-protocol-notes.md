# Chatto protocol notes (v0.5.0-beta.21)

Facts the bridge relies on, gathered from the Chatto source at the pinned tag.
Paths refer to the upstream repository. The public docs at docs.chatto.run
describe the older 0.4 line and are partly out of date (e.g. realtime v1).

## Transport

- ConnectRPC at `/api/connect/<pkg>.<Service>/<Method>`, unary only.
  `Authorization: Bearer <token>` works for session bearer tokens and bot API keys.
- Realtime: `GET /api/realtime` WebSocket, binary protobuf, **protocol version 4**.
  - The client sends exactly one `RealtimeSubscribe{protocol_version=4, bearer_token,
    resume_cursor?, initial_state}` and never sends anything else.
  - Server frames: `event`, `heartbeat{cursor?}`, `close{code, message, reconnect, retry_after}`,
    `caught_up{cursor, recovery}`, `snapshot{server, rooms, room_groups, users, active_calls}`.
  - Cursors expire 15 min after issue. An event's `cursor` (and a heartbeat's) may be
    retained only after every earlier frame was applied.
  - `recovery` is RESUMED, SNAPSHOT or LIVE_ONLY. Anything except RESUMED means
    the bridge may have missed events and must forward-fill from the timeline.
  - Origin header: empty or a valid browser origin is accepted. Send none.
  - Read limit 64 KiB, handshake timeout 10 s, credentials re-checked each minute.
  - Close codes: AUTHENTICATION_REQUIRED, SESSION_TERMINATED → login is dead;
    RESYNC_REQUIRED → drop the cursor; TEMPORARILY_UNAVAILABLE → retry after `retry_after`.
- Events are mostly invalidation signals: `message_posted` carries room, thread,
  reply, mentions and `body_plaintext`, but not attachments; hydrate via
  `MessageService.GetMessage`. **`RealtimeEvent.id == Message.id`** for posts.
  The actor is `RealtimeEvent.actor_id`.

## Messages

- The body is Markdown rendered by markdown-it (`html:false, linkify:true, breaks:true`),
  with setext headings, hr, reference links, images, inline HTML and **backslash
  escapes disabled**. Single newlines are hard breaks. GFM tables and footnotes are on.
  Strikethrough renders but the composer never produces it. There's no underline and no spoilers.
- Links are kept only for http, https, mailto and `/path`; autolinks need `http(s)://` or `www.`.
- Max body 10000 bytes. An empty body is allowed only with attachments.
- Mentions are plain `@login` text (also role names, `@here`, `@all`). Handles are `[A-Za-z0-9_-]`
  with interior dots. An `@` straight after an alphanumeric is not a mention. Mentions are
  ignored inside code, quotes and link text. Unknown handles are silently plain text.
- Message links: `https://host/chat/-/<roomId>[/<threadRootId>]/m/<eventId>`.
  Room IDs look like `R[a-zA-Z0-9]{14}`, event IDs `E[a-zA-Z0-9]{14}`.
- Edits: `UpdateMessage{room_id,event_id,body,update_mask}`. Authors have a 3 h window. No history.
- Delete: `DeleteMessage` produces a retraction; reads then carry `deleted_at` and GetMessage is NOT_FOUND.
- Replies: `in_reply_to` is attribution only. Threads: `thread_root_event_id` (the root must be a
  root), `create_thread`, `also_send_to_channel` (echo with its own ID, `echo_of_event_id`).
- RoomThreadingMode: REQUIRED auto-creates threads for roots and forbids plain replies to roots;
  DISABLED rejects threads and thread-targeting replies. DMs don't support threads.
- `idempotency_key`: a UUID, per actor, with a 30 min window. An exact retry returns the same
  message; the same key with different args returns AlreadyExists.
- Reactions use gemoji **shortcode names** (e.g. `+1`), never Unicode; max 20 distinct per user per message.

## Rooms, users and media

- RoomKind is CHANNEL or DM. A group DM is a DM with more than two `member_user_ids`. Bots cannot
  StartDM, and new DMs are 1:1 only. Rooms have no avatar.
- `RoomDirectoryService.ListRooms` (offset pages, max 100), `GetRoom`, `ListRoomGroups`.
  Room groups are spaces in the sidebar order.
- Universal rooms: every member is in them implicitly; no leave, no invites.
- Timeline: `RoomService.GetRoomEvents{room_id, limit≤500, before|after}` returns events oldest
  first with `start_cursor`/`end_cursor`. `ThreadService.GetThreadEvents` is the same per thread.
- Members: `RoomService.ListMembers` returns IDs only (max 500 per page); users come from
  `UserService.BatchGetUsers` (≤100). A user is a bot if `User.bot` is set.
- Upload: `AssetUploadService.CreateUpload{room_id, filename, content_type, size, sha256}`, then
  `UploadChunk{offset, content ≤ max_chunk_size, chunk_sha256}`, then `CompleteUpload`, which gives an `asset_id`.
  The default max upload is 25 MiB (`ServerService.GetRuntimeConfig`).
- Download: `MessageAssetUrl.url` may be relative; it carries an `access` ticket, so no auth header is
  needed. Follow redirects (S3). It expires after about 24 h; refresh via `AssetService.GetAsset`.
- Avatars: `User.avatar_url` is absolute and public.
- Typing: `RoomService.RefreshTypingIndicator` at most every 2 s; receivers time out after 6 s.
- Read state is **viewer-only**: `MarkRoomAsRead{up_to_event_id}` (root events only) and
  `MarkThreadAsRead`. Other users' receipts are not visible.
- Presence: `MyAccountService.RefreshPresence` every 30 s keeps you live.
  `User.presence_status` / `custom_status`; realtime `presence_changed` carries the actor.
- Calls: `voice_call_started/ended/participant_joined/left` realtime events, and
  `call_started/ended` in the timeline.
- Bots cannot be invited to rooms. They are added with `RoomService.AddMember` by the owner,
  or join via `JoinRoom` with room.join.

## Auth

- Password login: `POST /auth/login` JSON `{"login","password"}`, sent without the
  `X-Chatto-Authentication-Mode: cookie` header, returns
  `{token, refreshToken, expiresIn, refreshTokenExpiresIn, user{id,login}}`. No 2FA.
  It can be disabled server-side (`auth.direct_login=false` gives 403).
- Access tokens (`cht_AT…`) last 15 min by default; the refresh window is 90 days and slides.
- Refresh: `POST /oauth/token` JSON `{"grant_type":"refresh_token","refresh_token",
  "refresh_request_id":"<uuid>"}` (no client_id for password sessions) returns snake_case
  `{access_token, expires_in, refresh_token, refresh_token_expires_in}`. **Refresh
  tokens are one-time-use; presenting a stale one revokes the session.** Reusing the
  same request ID with the previous token replays a lost response.
- Logout: `POST /auth/logout` with the bearer token and `{"refreshToken"}`.
- OAuth: authorization code with PKCE S256, public clients only. `client_id` is a client
  metadata document URL (CIMD) that the server fetches; there's no registration. Redirects are HTTPS, or loopback for native clients.
- Bot API keys `cht_BK_…` never expire. Send them as `Authorization: Bearer`, and as
  `bearer_token` in the realtime subscribe frame.
- No Origin, Cookie or CSRF headers are needed for non-browser clients.
- Server version: `ServerDiscoveryService.GetServer().profile.version` (also `GET /.well-known/chatto`).

## Bots and permissions

- `BotService` (human tokens only): CreateBot{login, display_name} returns a one-time api_key.
  The owner can rename the bot, set its avatar (UserService), add it to rooms (`RoomService.AddMember`) and
  grant it permissions. Bots get **no baseline permissions**; grants are an allowlist capped
  at the owner's own permissions. Bots cannot StartDM, cannot be invited and cannot get roles.
- `AdminPermissionService.SetUserPermission{user_id, permission, decision, scope{kind,id}}`.
  Permissions a bridge bot needs: `message.read`, `message.post`, `message.post-in-thread`,
  `message.attach`, `message.react`, `message.echo`, `room.list`, `room.join`.
- No admin RPC creates human accounts. The Operator API (local unix socket, unauthenticated,
  disabled by default) can, and can import historical messages as anyone, but it has no live posting.
