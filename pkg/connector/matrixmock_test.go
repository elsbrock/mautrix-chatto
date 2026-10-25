package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// sentEvent is one event the bridge sent to the mock homeserver.
type sentEvent struct {
	ID        id.EventID
	RoomID    id.RoomID
	Sender    id.UserID
	Type      event.Type
	StateKey  *string
	Content   *event.Content
	Timestamp time.Time
}

// Message parses the content as a message event.
func (e *sentEvent) Message() *event.MessageEventContent {
	if err := e.Content.ParseRaw(e.Type); err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
		return nil
	}
	msg, _ := e.Content.Parsed.(*event.MessageEventContent)
	return msg
}

// Reaction parses the content as a reaction.
func (e *sentEvent) Reaction() *event.ReactionEventContent {
	if err := e.Content.ParseRaw(e.Type); err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
		return nil
	}
	r, _ := e.Content.Parsed.(*event.ReactionEventContent)
	return r
}

// mockMatrix is an in-memory homeserver standing in for bridgev2's Matrix
// connector. It records everything the bridge sends.
type mockMatrix struct {
	br *bridgev2.Bridge

	mu        sync.Mutex
	nextID    int
	events    []*sentEvent
	rooms     map[id.RoomID]*mockRoom
	media     map[id.ContentURIString][]byte
	typing    []string
	reads     map[string]id.EventID // user/room → event
	presences map[id.UserID]mautrix.ReqPresence
	intents   map[id.UserID]*mockIntent
	notify    chan struct{}
}

type mockRoom struct {
	creator id.UserID
	req     *mautrix.ReqCreateRoom
	members map[id.UserID]event.Membership
	state   map[string]*event.Content
}

func newMockMatrix() *mockMatrix {
	return &mockMatrix{
		rooms:     make(map[id.RoomID]*mockRoom),
		media:     make(map[id.ContentURIString][]byte),
		reads:     make(map[string]id.EventID),
		presences: make(map[id.UserID]mautrix.ReqPresence),
		intents:   make(map[id.UserID]*mockIntent),
		notify:    make(chan struct{}, 10000),
	}
}

var _ bridgev2.MatrixConnector = (*mockMatrix)(nil)

func (m *mockMatrix) poke() {
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

func (m *mockMatrix) Init(br *bridgev2.Bridge)        { m.br = br }
func (m *mockMatrix) Start(ctx context.Context) error { return nil }
func (m *mockMatrix) PreStop()                        {}
func (m *mockMatrix) Stop()                           {}

func (m *mockMatrix) GetCapabilities() *bridgev2.MatrixCapabilities {
	return &bridgev2.MatrixCapabilities{AutoJoinInvites: true}
}

const ghostPrefix = "@chatto_"

func (m *mockMatrix) ParseGhostMXID(userID id.UserID) (networkid.UserID, bool) {
	s := string(userID)
	if !strings.HasPrefix(s, ghostPrefix) || !strings.HasSuffix(s, ":test") {
		return "", false
	}
	return networkid.UserID(strings.TrimSuffix(strings.TrimPrefix(s, ghostPrefix), ":test")), true
}

func (m *mockMatrix) FormatGhostMXID(userID networkid.UserID) id.UserID {
	return id.UserID(ghostPrefix + string(userID) + ":test")
}

func (m *mockMatrix) intent(userID id.UserID, puppet bool) *mockIntent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if in, ok := m.intents[userID]; ok {
		return in
	}
	in := &mockIntent{m: m, mxid: userID, doublePuppet: puppet}
	m.intents[userID] = in
	return in
}

func (m *mockMatrix) GhostIntent(userID networkid.UserID) bridgev2.MatrixAPI {
	return m.intent(m.FormatGhostMXID(userID), false)
}

func (m *mockMatrix) NewUserIntent(ctx context.Context, userID id.UserID, accessToken string) (bridgev2.MatrixAPI, string, error) {
	return nil, "", errors.New("double puppeting not supported in tests")
}

func (m *mockMatrix) BotIntent() bridgev2.MatrixAPI { return m.intent("@bot:test", false) }

func (m *mockMatrix) SendBridgeStatus(ctx context.Context, state *status.BridgeState) error {
	return nil
}

func (m *mockMatrix) SendMessageStatus(ctx context.Context, st *bridgev2.MessageStatus, evt *bridgev2.MessageStatusEventInfo) {
}

func (m *mockMatrix) GenerateContentURI(ctx context.Context, mediaID networkid.MediaID) (id.ContentURIString, error) {
	return "", errors.New("not supported")
}

func (m *mockMatrix) ParseContentURI(ctx context.Context, contentURI id.ContentURIString) (networkid.MediaID, error) {
	return nil, errors.New("not supported")
}

func (m *mockMatrix) GetPowerLevels(ctx context.Context, roomID id.RoomID) (*event.PowerLevelsEventContent, error) {
	return &event.PowerLevelsEventContent{}, nil
}

func (m *mockMatrix) GetMembers(ctx context.Context, roomID id.RoomID) (map[id.UserID]*event.MemberEventContent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[id.UserID]*event.MemberEventContent)
	if r, ok := m.rooms[roomID]; ok {
		for u, ms := range r.members {
			out[u] = &event.MemberEventContent{Membership: ms}
		}
	}
	return out, nil
}

func (m *mockMatrix) GetMemberInfo(ctx context.Context, roomID id.RoomID, userID id.UserID) (*event.MemberEventContent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rooms[roomID]; ok {
		if ms, ok := r.members[userID]; ok {
			return &event.MemberEventContent{Membership: ms}, nil
		}
	}
	return &event.MemberEventContent{Membership: event.MembershipLeave}, nil
}

func (m *mockMatrix) BatchSend(ctx context.Context, roomID id.RoomID, req *mautrix.ReqBeeperBatchSend, extras []*bridgev2.MatrixSendExtra) (*mautrix.RespBeeperBatchSend, error) {
	return nil, errors.New("batch sending not supported")
}

func (m *mockMatrix) GenerateDeterministicRoomID(portalKey networkid.PortalKey) id.RoomID { return "" }

func (m *mockMatrix) GenerateDeterministicEventID(roomID id.RoomID, portalKey networkid.PortalKey, messageID networkid.MessageID, partID networkid.PartID) id.EventID {
	return ""
}

func (m *mockMatrix) GenerateReactionEventID(roomID id.RoomID, targetMessage *database.Message, sender networkid.UserID, emojiID networkid.EmojiID) id.EventID {
	return ""
}

func (m *mockMatrix) ServerName() string { return "test" }

func (m *mockMatrix) record(evt *sentEvent) id.EventID {
	m.mu.Lock()
	m.nextID++
	evt.ID = id.EventID(fmt.Sprintf("$ev%d:test", m.nextID))
	m.events = append(m.events, evt)
	if evt.StateKey != nil {
		if r, ok := m.rooms[evt.RoomID]; ok {
			r.state[evt.Type.Type+"|"+*evt.StateKey] = evt.Content
			if evt.Type == event.StateMember {
				_ = evt.Content.ParseRaw(evt.Type)
				if mc, ok := evt.Content.Parsed.(*event.MemberEventContent); ok {
					r.members[id.UserID(*evt.StateKey)] = mc.Membership
				}
			}
		}
	}
	m.mu.Unlock()
	m.poke()
	return evt.ID
}

// Events returns a snapshot of recorded events matching the filter.
func (m *mockMatrix) Events(filter func(*sentEvent) bool) []*sentEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*sentEvent
	for _, e := range m.events {
		if filter == nil || filter(e) {
			out = append(out, e)
		}
	}
	return out
}

// mockIntent is a Matrix user (ghost, bot or puppet).
type mockIntent struct {
	m            *mockMatrix
	mxid         id.UserID
	doublePuppet bool
	displayName  string
	avatar       id.ContentURIString
}

var _ bridgev2.MatrixAPI = (*mockIntent)(nil)

func (i *mockIntent) GetMXID() id.UserID   { return i.mxid }
func (i *mockIntent) IsDoublePuppet() bool { return i.doublePuppet }

func (i *mockIntent) SendMessage(ctx context.Context, roomID id.RoomID, eventType event.Type, content *event.Content, extra *bridgev2.MatrixSendExtra) (*mautrix.RespSendEvent, error) {
	evt := &sentEvent{RoomID: roomID, Sender: i.mxid, Type: eventType, Content: content}
	if extra != nil {
		evt.Timestamp = extra.Timestamp
	}
	return &mautrix.RespSendEvent{EventID: i.m.record(evt)}, nil
}

func (i *mockIntent) SendState(ctx context.Context, roomID id.RoomID, eventType event.Type, stateKey string, content *event.Content, ts time.Time) (*mautrix.RespSendEvent, error) {
	return &mautrix.RespSendEvent{EventID: i.m.record(&sentEvent{RoomID: roomID, Sender: i.mxid, Type: eventType, StateKey: &stateKey, Content: content, Timestamp: ts})}, nil
}

func (i *mockIntent) MarkRead(ctx context.Context, roomID id.RoomID, eventID id.EventID, ts time.Time) error {
	i.m.mu.Lock()
	i.m.reads[string(i.mxid)+"/"+string(roomID)] = eventID
	i.m.mu.Unlock()
	i.m.poke()
	return nil
}

func (i *mockIntent) MarkUnread(ctx context.Context, roomID id.RoomID, unread bool) error { return nil }

func (i *mockIntent) MarkTyping(ctx context.Context, roomID id.RoomID, typingType bridgev2.TypingType, timeout time.Duration) error {
	i.m.mu.Lock()
	i.m.typing = append(i.m.typing, fmt.Sprintf("%s/%s/%v", i.mxid, roomID, timeout > 0))
	i.m.mu.Unlock()
	i.m.poke()
	return nil
}

func (i *mockIntent) DownloadMedia(ctx context.Context, uri id.ContentURIString, file *event.EncryptedFileInfo) ([]byte, error) {
	i.m.mu.Lock()
	defer i.m.mu.Unlock()
	data, ok := i.m.media[uri]
	if !ok {
		return nil, mautrix.MNotFound
	}
	return data, nil
}

func (i *mockIntent) DownloadMediaToFile(ctx context.Context, uri id.ContentURIString, file *event.EncryptedFileInfo, writable bool, callback func(*os.File) error) error {
	return errors.New("not supported")
}

func (i *mockIntent) UploadMedia(ctx context.Context, roomID id.RoomID, data []byte, fileName, mimeType string) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	i.m.mu.Lock()
	defer i.m.mu.Unlock()
	i.m.nextID++
	uri := id.ContentURIString(fmt.Sprintf("mxc://test/media%d", i.m.nextID))
	i.m.media[uri] = data
	return uri, nil, nil
}

func (i *mockIntent) UploadMediaStream(ctx context.Context, roomID id.RoomID, size int64, requireFile bool, cb bridgev2.FileStreamCallback) (id.ContentURIString, *event.EncryptedFileInfo, error) {
	return "", nil, errors.New("not supported")
}

func (i *mockIntent) SetDisplayName(ctx context.Context, name string) error {
	i.displayName = name
	return nil
}

func (i *mockIntent) SetAvatarURL(ctx context.Context, avatarURL id.ContentURIString) error {
	i.avatar = avatarURL
	return nil
}

func (i *mockIntent) SetExtraProfileMeta(ctx context.Context, data any) error { return nil }
func (i *mockIntent) SetProfile(ctx context.Context, data any) error          { return nil }

func (i *mockIntent) CreateRoom(ctx context.Context, req *mautrix.ReqCreateRoom) (id.RoomID, error) {
	i.m.mu.Lock()
	i.m.nextID++
	roomID := id.RoomID(fmt.Sprintf("!room%d:test", i.m.nextID))
	r := &mockRoom{creator: i.mxid, req: req, members: map[id.UserID]event.Membership{i.mxid: event.MembershipJoin}, state: map[string]*event.Content{}}
	for _, u := range req.Invite {
		r.members[u] = event.MembershipJoin
	}
	for _, st := range req.InitialState {
		r.state[st.Type.Type+"|"+st.GetStateKey()] = &st.Content
		if st.Type == event.StateMember {
			_ = st.Content.ParseRaw(st.Type)
			if mc, ok := st.Content.Parsed.(*event.MemberEventContent); ok {
				r.members[id.UserID(st.GetStateKey())] = mc.Membership
			}
		}
	}
	i.m.rooms[roomID] = r
	i.m.mu.Unlock()
	i.m.poke()
	return roomID, nil
}

func (i *mockIntent) DeleteRoom(ctx context.Context, roomID id.RoomID, puppetsOnly bool) error {
	i.m.mu.Lock()
	delete(i.m.rooms, roomID)
	i.m.mu.Unlock()
	i.m.poke()
	return nil
}

func (i *mockIntent) EnsureJoined(ctx context.Context, roomID id.RoomID, params ...bridgev2.EnsureJoinedParams) error {
	i.m.mu.Lock()
	if r, ok := i.m.rooms[roomID]; ok {
		r.members[i.mxid] = event.MembershipJoin
	}
	i.m.mu.Unlock()
	return nil
}

func (i *mockIntent) EnsureInvited(ctx context.Context, roomID id.RoomID, userID id.UserID) error {
	return nil
}

func (i *mockIntent) TagRoom(ctx context.Context, roomID id.RoomID, tag event.RoomTag, isTagged bool) error {
	return nil
}

func (i *mockIntent) MuteRoom(ctx context.Context, roomID id.RoomID, until time.Time) error {
	return nil
}

func (i *mockIntent) GetEvent(ctx context.Context, roomID id.RoomID, eventID id.EventID) (*event.Event, error) {
	return nil, mautrix.MNotFound
}

// httpClientNoRedirectLimit is the HTTP client given to the connector.
var httpClientNoRedirectLimit = &http.Client{Timeout: 30 * time.Second}

func (i *mockIntent) SetPresence(ctx context.Context, presence mautrix.ReqPresence) error {
	i.m.mu.Lock()
	i.m.presences[i.mxid] = presence
	i.m.mu.Unlock()
	i.m.poke()
	return nil
}
