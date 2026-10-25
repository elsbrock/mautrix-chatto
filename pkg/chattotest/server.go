// Package chattotest is an in-memory fake Chatto server for tests. It
// implements the subset of the ConnectRPC API, HTTP auth endpoints and the
// realtime v4 WebSocket protocol that the bridge uses, built on the
// generated connect handlers so request and response shapes match the real
// server.
package chattotest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/admin/v1/adminv1connect"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1/apiv1connect"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/discovery/v1/discoveryv1connect"
	realtimev1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/realtime/v1"
)

// Version is the version the fake server reports.
const Version = "v0.5.0-beta.21"

// User is a fake account.
type User struct {
	ID          string
	Login       string
	DisplayName string
	Password    string
	BotOwner    string // non-empty for bots
	AvatarKey   string
	Presence    apiv1.PresenceStatus
	Status      *apiv1.CustomUserStatus
}

// Room is a fake room.
type Room struct {
	ID            string
	Kind          apiv1.RoomKind
	Name          string
	Description   string
	GroupID       string
	Archived      bool
	Universal     bool
	ThreadingMode apiv1.RoomThreadingMode
	Members       []string
	CreatedAt     time.Time
}

// Group is a fake room group.
type Group struct {
	ID          string
	Name        string
	Description string
	RoomIDs     []string
}

// Message is a fake message.
type Message struct {
	ID          string
	RoomID      string
	ActorID     string
	Body        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   time.Time
	InReplyTo   string
	ThreadRoot  string
	Attachments []*Attachment
	// reactions: emoji → user IDs in reaction order
	Reactions map[string][]string
	reactOrd  []string
}

// Attachment is a fake attachment.
type Attachment struct {
	ID          string
	Filename    string
	ContentType string
	Data        []byte
	Processing  bool
	Description string
}

// Permission records one SetUserPermission call.
type Permission struct {
	UserID, Permission string
	Decision           int32
	ScopeKind          int32
	ScopeID            string
}

type session struct {
	userID        string
	accessExpiry  time.Time
	refreshToken  string
	usedRefreshes map[string]bool
}

// Server is the fake Chatto server.
type Server struct {
	HTTP *httptest.Server

	// AccessTTL is the lifetime of issued access tokens.
	AccessTTL time.Duration
	// HeartbeatInterval is how often realtime heartbeats are sent.
	HeartbeatInterval time.Duration
	// MaxUploadSize is reported in the runtime config.
	MaxUploadSize int64
	// MaxChunkSize is announced for uploads.
	MaxChunkSize int32
	// DirectLogin enables password login.
	DirectLogin bool
	// PublishBeforeResponse makes CreateMessage publish its realtime event
	// and wait this long before responding, to reproduce the echo race.
	PublishBeforeResponse time.Duration

	mu          sync.Mutex
	users       map[string]*User
	rooms       map[string]*Room
	groups      map[string]*Group
	groupOrder  []string
	messages    map[string]*Message // by ID
	timeline    map[string][]string // room ID → root message IDs in order (incl. non-message events)
	threads     map[string][]string // root ID → reply IDs
	sessions    map[string]*session // access token → session
	refresh     map[string]*session // refresh token → session
	botKeys     map[string]string   // key → user ID
	uploads     map[string]*upload
	assets      map[string]*Attachment
	readMarkers map[string]string // userID/roomID → event ID
	idem        map[string]string // userID/key → message ID
	Permissions []Permission
	TypingCalls []string // "userID/roomID" per RefreshTypingIndicator call
	Calls       []string // procedures called, in order

	idCounter   atomic.Int64
	cursorFloor int
	events      []*loggedEvent
	subs        map[*subscription]struct{}
}

type upload struct {
	id, roomID, filename, contentType, sha string
	size                                   int64
	data                                   []byte
	assetID                                string
}

// New starts a fake server. Call Close when done.
func New() *Server {
	s := &Server{
		AccessTTL:         15 * time.Minute,
		HeartbeatInterval: time.Hour,
		MaxUploadSize:     25 << 20,
		MaxChunkSize:      1 << 20,
		DirectLogin:       true,
		users:             make(map[string]*User),
		rooms:             make(map[string]*Room),
		groups:            make(map[string]*Group),
		messages:          make(map[string]*Message),
		timeline:          make(map[string][]string),
		threads:           make(map[string][]string),
		sessions:          make(map[string]*session),
		refresh:           make(map[string]*session),
		botKeys:           make(map[string]string),
		uploads:           make(map[string]*upload),
		assets:            make(map[string]*Attachment),
		readMarkers:       make(map[string]string),
		idem:              make(map[string]string),
		subs:              make(map[*subscription]struct{}),
	}
	mux := http.NewServeMux()
	opts := []connect.HandlerOption{connect.WithInterceptors(&authInterceptor{s: s})}
	mount := func(path string, h http.Handler) {
		mux.Handle("/api/connect"+path, http.StripPrefix("/api/connect", h))
	}
	mount(discoveryv1connect.NewServerDiscoveryServiceHandler(&discoveryService{s: s}, opts...))
	mount(apiv1connect.NewViewerServiceHandler(&viewerService{s: s}, opts...))
	mount(apiv1connect.NewMyAccountServiceHandler(&accountService{s: s}, opts...))
	mount(apiv1connect.NewServerServiceHandler(&serverService{s: s}, opts...))
	mount(apiv1connect.NewUserServiceHandler(&userService{s: s}, opts...))
	mount(apiv1connect.NewRoomDirectoryServiceHandler(&directoryService{s: s}, opts...))
	mount(apiv1connect.NewRoomServiceHandler(&roomService{s: s}, opts...))
	mount(apiv1connect.NewThreadServiceHandler(&threadService{s: s}, opts...))
	mount(apiv1connect.NewMessageServiceHandler(&messageService{s: s}, opts...))
	mount(apiv1connect.NewAssetUploadServiceHandler(&uploadService{s: s}, opts...))
	mount(apiv1connect.NewRoomInvitationServiceHandler(&invitationService{s: s}, opts...))
	mount(adminv1connect.NewAdminPermissionServiceHandler(&adminPermissionService{s: s}, opts...))
	mux.HandleFunc("POST /auth/login", s.handleLogin)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/realtime", s.handleRealtime)
	mux.HandleFunc("GET /assets/files/{id}", s.handleAsset)
	mux.HandleFunc("GET /assets/server/{key}", s.handleAvatar)
	s.HTTP = httptest.NewServer(mux)
	return s
}

// URL is the base URL of the server.
func (s *Server) URL() string { return s.HTTP.URL }

// Close stops the server and all subscriptions.
func (s *Server) Close() {
	s.mu.Lock()
	for sub := range s.subs {
		sub.cancel()
	}
	s.mu.Unlock()
	s.HTTP.CloseClientConnections()
	s.HTTP.Close()
}

func (s *Server) newID(prefix string) string {
	return fmt.Sprintf("%s%014d", prefix, s.idCounter.Add(1))
}

// AddUser creates a human account.
func (s *Server) AddUser(login, displayName, password string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &User{ID: s.newID("U"), Login: login, DisplayName: displayName, Password: password, Presence: apiv1.PresenceStatus_PRESENCE_STATUS_ONLINE}
	s.users[u.ID] = u
	return u
}

// AddBot creates a bot account owned by owner and returns its API key.
func (s *Server) AddBot(login, displayName, ownerID string) (*User, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &User{ID: s.newID("U"), Login: login, DisplayName: displayName, BotOwner: ownerID}
	s.users[u.ID] = u
	key := "cht_BK_" + s.newID("K")
	s.botKeys[key] = u.ID
	return u, key
}

// IssueSession creates a bearer session for a user and returns its tokens.
func (s *Server) IssueSession(userID string) (access, refresh string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issueSessionLocked(&session{userID: userID, usedRefreshes: map[string]bool{}})
}

func (s *Server) issueSessionLocked(sess *session) (string, string) {
	access := "cht_AT_" + s.newID("A")
	refresh := "cht_RT_" + s.newID("R")
	sess.accessExpiry = time.Now().Add(s.AccessTTL)
	sess.refreshToken = refresh
	s.sessions[access] = sess
	s.refresh[refresh] = sess
	return access, refresh
}

// ExpireAccessTokens makes every issued access token invalid.
func (s *Server) ExpireAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		sess.accessExpiry = time.Now().Add(-time.Second)
	}
}

// AddChannel creates a channel with members.
func (s *Server) AddChannel(name, groupID string, members ...string) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Room{ID: s.newID("R"), Kind: apiv1.RoomKind_ROOM_KIND_CHANNEL, Name: name, GroupID: groupID,
		ThreadingMode: apiv1.RoomThreadingMode_ROOM_THREADING_MODE_ENABLED, Members: slices.Clone(members), CreatedAt: time.Now()}
	s.rooms[r.ID] = r
	if g := s.groups[groupID]; g != nil {
		g.RoomIDs = append(g.RoomIDs, r.ID)
	}
	return r
}

// AddDM creates a DM between users.
func (s *Server) AddDM(members ...string) *Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := &Room{ID: s.newID("R"), Kind: apiv1.RoomKind_ROOM_KIND_DM, Members: slices.Clone(members), CreatedAt: time.Now()}
	s.rooms[r.ID] = r
	return r
}

// AddGroup creates a room group.
func (s *Server) AddGroup(name string) *Group {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := &Group{ID: s.newID("G"), Name: name}
	s.groups[g.ID] = g
	s.groupOrder = append(s.groupOrder, g.ID)
	return g
}

// Room returns a copy of a room's state.
func (s *Server) Room(id string) Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.rooms[id]
}

// Message returns a copy of a message, or nil.
func (s *Server) Message(id string) *Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[id]
	if !ok {
		return nil
	}
	cp := *m
	cp.Reactions = make(map[string][]string, len(m.Reactions))
	for k, v := range m.Reactions {
		cp.Reactions[k] = slices.Clone(v)
	}
	return &cp
}

// Messages returns all messages of a room (roots and replies) in creation order.
func (s *Server) Messages(roomID string) []*Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Message
	for _, m := range s.messages {
		if m.RoomID == roomID {
			cp := *m
			out = append(out, &cp)
		}
	}
	slices.SortFunc(out, func(a, b *Message) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// CallCount counts calls to a procedure suffix such as "CreateMessage".
func (s *Server) CallCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.Calls {
		if strings.HasSuffix(c, "/"+method) {
			n++
		}
	}
	return n
}

// ReadMarker returns a user's read marker in a room.
func (s *Server) ReadMarker(userID, roomID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readMarkers[userID+"/"+roomID]
}

// PostMessage creates a message as actor and publishes it, as if sent from
// another Chatto client.
func (s *Server) PostMessage(actorID, roomID, body string, opts ...func(*Message)) *Message {
	s.mu.Lock()
	m := &Message{ID: s.newID("E"), RoomID: roomID, ActorID: actorID, Body: body, CreatedAt: time.Now(), Reactions: map[string][]string{}}
	for _, o := range opts {
		o(m)
	}
	s.storeMessageLocked(m)
	s.mu.Unlock()
	s.publishMessagePosted(m)
	return m
}

// InThread makes PostMessage post into a thread.
func InThread(rootID string) func(*Message) { return func(m *Message) { m.ThreadRoot = rootID } }

// ReplyTo makes PostMessage reply to a message.
func ReplyTo(id string) func(*Message) { return func(m *Message) { m.InReplyTo = id } }

// WithAttachment adds an attachment to PostMessage.
func (s *Server) WithAttachment(filename, contentType string, data []byte, processing bool) func(*Message) {
	return func(m *Message) {
		a := &Attachment{ID: s.newID("A"), Filename: filename, ContentType: contentType, Data: data, Processing: processing}
		s.assets[a.ID] = a
		m.Attachments = append(m.Attachments, a)
	}
}

func (s *Server) storeMessageLocked(m *Message) {
	s.messages[m.ID] = m
	if m.ThreadRoot != "" {
		s.threads[m.ThreadRoot] = append(s.threads[m.ThreadRoot], m.ID)
	} else {
		s.timeline[m.RoomID] = append(s.timeline[m.RoomID], m.ID)
	}
}

// EditMessage edits a message as if from another client and publishes it.
func (s *Server) EditMessage(id, body string) {
	s.mu.Lock()
	m := s.messages[id]
	m.Body = body
	m.UpdatedAt = time.Now()
	s.mu.Unlock()
	s.Publish(m.RoomID, m.ActorID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_MessageEdited{
		MessageEdited: &realtimev1.MessageEditedEvent{RoomId: m.RoomID, MessageEventId: id},
	}})
}

// FinishProcessing marks an attachment processed and publishes the event.
func (s *Server) FinishProcessing(messageID, attachmentID string) {
	s.mu.Lock()
	m := s.messages[messageID]
	for _, a := range m.Attachments {
		if a.ID == attachmentID {
			a.Processing = false
		}
	}
	s.mu.Unlock()
	s.Publish(m.RoomID, "", &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_AssetProcessingSucceeded{
		AssetProcessingSucceeded: &realtimev1.AssetProcessingSucceededEvent{AssetId: attachmentID, MessageEventId: messageID, RoomId: m.RoomID},
	}})
}

// RetractMessage deletes a message as if from another client.
func (s *Server) RetractMessage(id string) {
	s.mu.Lock()
	m := s.messages[id]
	m.DeletedAt = time.Now()
	s.mu.Unlock()
	s.Publish(m.RoomID, m.ActorID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_MessageRetracted{
		MessageRetracted: &realtimev1.MessageRetractedEvent{RoomId: m.RoomID, MessageEventId: id},
	}})
}

// React adds a reaction as actor and publishes it.
func (s *Server) React(actorID, messageID, shortcode string) {
	s.mu.Lock()
	m := s.messages[messageID]
	addReactionLocked(m, actorID, shortcode)
	s.mu.Unlock()
	s.Publish(m.RoomID, actorID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_ReactionAdded{
		ReactionAdded: &realtimev1.ReactionAddedEvent{RoomId: m.RoomID, MessageEventId: messageID, Emoji: shortcode},
	}})
}

func addReactionLocked(m *Message, userID, emoji string) bool {
	if slices.Contains(m.Reactions[emoji], userID) {
		return false
	}
	if _, ok := m.Reactions[emoji]; !ok {
		m.reactOrd = append(m.reactOrd, emoji)
	}
	m.Reactions[emoji] = append(m.Reactions[emoji], userID)
	return true
}

// PublishTyping publishes a typing event.
func (s *Server) PublishTyping(actorID, roomID string) {
	s.Publish(roomID, actorID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_UserTyping{
		UserTyping: &realtimev1.UserTypingEvent{RoomId: roomID},
	}})
}

// UpdateRoom renames a room and publishes the change.
func (s *Server) UpdateRoom(roomID, name, description string) {
	s.mu.Lock()
	r := s.rooms[roomID]
	r.Name, r.Description = name, description
	s.mu.Unlock()
	s.Publish(roomID, "", &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_RoomUpdated{
		RoomUpdated: &realtimev1.RoomUpdatedEvent{RoomId: roomID, Name: name, Description: description},
	}})
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// userFromRequest resolves a bearer token.
func (s *Server) userFromToken(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if uid, ok := s.botKeys[token]; ok {
		return uid, true
	}
	sess, ok := s.sessions[token]
	if !ok || time.Now().After(sess.accessExpiry) {
		return "", false
	}
	return sess.userID, true
}

type ctxKey struct{}

// viewerID returns the authenticated user of a request.
func viewerID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

type authInterceptor struct{ s *Server }

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		a.s.mu.Lock()
		a.s.Calls = append(a.s.Calls, req.Spec().Procedure)
		a.s.mu.Unlock()
		if strings.HasPrefix(req.Spec().Procedure, "/chatto.discovery.") {
			return next(ctx, req)
		}
		token, ok := strings.CutPrefix(req.Header().Get("Authorization"), "Bearer ")
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("missing bearer token"))
		}
		uid, ok := a.s.userFromToken(token)
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid or expired token"))
		}
		return next(context.WithValue(ctx, ctxKey{}, uid), req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.DirectLogin {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "direct login disabled"})
		return
	}
	var req struct{ Login, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	s.mu.Lock()
	var user *User
	for _, u := range s.users {
		if strings.EqualFold(u.Login, req.Login) && u.Password != "" && u.Password == req.Password {
			user = u
		}
	}
	if user == nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid credentials"})
		return
	}
	access, refresh := s.issueSessionLocked(&session{userID: user.ID, usedRefreshes: map[string]bool{}})
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "user": map[string]string{"id": user.ID, "login": user.Login},
		"token": access, "refreshToken": refresh,
		"expiresIn": int(s.AccessTTL.Seconds()), "refreshTokenExpiresIn": 90 * 24 * 3600,
	})
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.GrantType != "refresh_token" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.refresh[req.RefreshToken]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	if sess.refreshToken != req.RefreshToken {
		// Reuse of a rotated token revokes the whole session.
		for tok, other := range s.sessions {
			if other == sess {
				delete(s.sessions, tok)
			}
		}
		for tok, other := range s.refresh {
			if other == sess {
				delete(s.refresh, tok)
			}
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		return
	}
	access, refresh := s.issueSessionLocked(sess)
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": int(s.AccessTTL.Seconds()),
		"refresh_token": refresh, "refresh_token_expires_in": 90 * 24 * 3600,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	if sess, ok := s.sessions[token]; ok {
		for tok, other := range s.sessions {
			if other == sess {
				delete(s.sessions, tok)
			}
		}
		for tok, other := range s.refresh {
			if other == sess {
				delete(s.refresh, tok)
			}
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("access") == "" {
		http.Error(w, "missing ticket", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	a, ok := s.assets[r.PathValue("id")]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.ContentType)
	_, _ = w.Write(a.Data)
}

func (s *Server) handleAvatar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write([]byte("\x89PNG fake avatar " + r.PathValue("key")))
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
