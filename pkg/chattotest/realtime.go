package chattotest

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
	realtimev1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/realtime/v1"
)

type loggedEvent struct {
	seq    int
	roomID string
	evt    *realtimev1.RealtimeEvent
}

type subscription struct {
	userID string
	out    chan *realtimev1.RealtimeServerFrame
	cancel context.CancelFunc
}

func (sub *subscription) send(frame *realtimev1.RealtimeServerFrame) {
	select {
	case sub.out <- frame:
	default:
		// Slow consumer: drop the connection like a real server would.
		sub.cancel()
	}
}

// cursorMin is the lowest sequence number a cursor may resume from;
// ExpireCursors raises it.
var cursorPrefix = "c"

// Subscribers returns the number of live realtime subscriptions.
func (s *Server) Subscribers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

// WaitForSubscribers waits until at least n subscriptions are live.
func (s *Server) WaitForSubscribers(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.Subscribers() >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// CloseRealtime sends a close frame to every subscriber.
func (s *Server) CloseRealtime(code realtimev1.RealtimeCloseCode, reconnect bool, retryAfter time.Duration) {
	s.mu.Lock()
	subs := make([]*subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()
	for _, sub := range subs {
		sub.send(&realtimev1.RealtimeServerFrame{Frame: &realtimev1.RealtimeServerFrame_Close{Close: &realtimev1.RealtimeClose{
			Code: code, Message: "test close", Reconnect: reconnect, RetryAfter: durationpb.New(retryAfter),
		}}})
	}
}

// DropRealtime abruptly closes every realtime connection.
func (s *Server) DropRealtime() {
	s.mu.Lock()
	for sub := range s.subs {
		sub.cancel()
	}
	s.mu.Unlock()
}

// ExpireCursors makes all previously issued cursors unusable, so the next
// subscription gets a snapshot.
func (s *Server) ExpireCursors() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursorFloor = len(s.events) + 1
}

// Publish appends an event to the log and delivers it to every subscriber
// who is a member of roomID (or to everyone when roomID is empty).
func (s *Server) Publish(roomID, actorID string, evt *realtimev1.RealtimeEvent) {
	s.mu.Lock()
	if evt.Id == "" {
		evt.Id = s.newID("V")
	}
	if evt.CreatedAt == nil {
		evt.CreatedAt = timestamppb.Now()
	}
	if actorID != "" {
		evt.ActorId = &actorID
	}
	le := &loggedEvent{seq: len(s.events) + 1, roomID: roomID, evt: evt}
	s.events = append(s.events, le)
	var targets []*subscription
	for sub := range s.subs {
		if s.canSeeLocked(sub.userID, roomID) {
			targets = append(targets, sub)
		}
	}
	s.mu.Unlock()
	for _, sub := range targets {
		sub.send(eventFrame(le))
	}
}

func eventFrame(le *loggedEvent) *realtimev1.RealtimeServerFrame {
	evt := proto.Clone(le.evt).(*realtimev1.RealtimeEvent)
	cursor := cursorPrefix + strconv.Itoa(le.seq)
	evt.Cursor = &cursor
	return &realtimev1.RealtimeServerFrame{Frame: &realtimev1.RealtimeServerFrame_Event{Event: evt}}
}

func (s *Server) canSeeLocked(userID, roomID string) bool {
	if roomID == "" {
		return true
	}
	r, ok := s.rooms[roomID]
	return ok && slices.Contains(r.Members, userID)
}

func (s *Server) publishMessagePosted(m *Message) {
	s.mu.Lock()
	kind := s.rooms[m.RoomID].Kind
	s.mu.Unlock()
	body := m.Body
	s.Publish(m.RoomID, m.ActorID, &realtimev1.RealtimeEvent{
		Id:        m.ID,
		CreatedAt: timestamppb.New(m.CreatedAt),
		Event: &realtimev1.RealtimeEvent_MessagePosted{MessagePosted: &realtimev1.MessagePostedEvent{
			RoomId:            m.RoomID,
			InReplyTo:         m.InReplyTo,
			ThreadRootEventId: m.ThreadRoot,
			BodyPlaintext:     &body,
			RoomKind:          kind,
		}},
	})
}

func (s *Server) handleRealtime(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ctx, readCancel := context.WithTimeout(ctx, 10*time.Second)
	typ, data, err := conn.Read(ctx)
	readCancel()
	if err != nil || typ != websocket.MessageBinary {
		return
	}
	var sub realtimev1.RealtimeSubscribe
	writeFrame := func(f *realtimev1.RealtimeServerFrame) error {
		b, _ := proto.Marshal(f)
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageBinary, b)
	}
	closeWith := func(code realtimev1.RealtimeCloseCode, reconnect bool) {
		_ = writeFrame(&realtimev1.RealtimeServerFrame{Frame: &realtimev1.RealtimeServerFrame_Close{Close: &realtimev1.RealtimeClose{
			Code: code, Reconnect: reconnect,
		}}})
		_ = conn.Close(websocket.StatusPolicyViolation, code.String())
	}
	if err := proto.Unmarshal(data, &sub); err != nil {
		closeWith(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_INVALID_REQUEST, false)
		return
	}
	if sub.GetProtocolVersion() != 4 {
		closeWith(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_UNSUPPORTED_PROTOCOL, false)
		return
	}
	userID, ok := s.userFromToken(sub.GetBearerToken())
	if !ok {
		closeWith(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_AUTHENTICATION_REQUIRED, true)
		return
	}
	connCtx, connCancel := context.WithCancel(r.Context())
	defer connCancel()
	subscr := &subscription{userID: userID, out: make(chan *realtimev1.RealtimeServerFrame, 1024), cancel: connCancel}

	// Register first so nothing published during recovery is lost, then
	// send the replay or snapshot ahead of anything queued.
	s.mu.Lock()
	s.subs[subscr] = struct{}{}
	var recovery []*realtimev1.RealtimeServerFrame
	result := realtimev1.RealtimeRecovery_REALTIME_RECOVERY_SNAPSHOT
	if c, ok := strings.CutPrefix(sub.GetResumeCursor(), cursorPrefix); ok {
		if seq, err := strconv.Atoi(c); err == nil && seq >= s.cursorFloor && seq <= len(s.events) {
			result = realtimev1.RealtimeRecovery_REALTIME_RECOVERY_RESUMED
			for _, le := range s.events[seq:] {
				if s.canSeeLocked(userID, le.roomID) {
					recovery = append(recovery, eventFrame(le))
				}
			}
		}
	}
	if result != realtimev1.RealtimeRecovery_REALTIME_RECOVERY_RESUMED {
		if sub.GetInitialState() == realtimev1.RealtimeInitialState_REALTIME_INITIAL_STATE_LIVE_ONLY {
			result = realtimev1.RealtimeRecovery_REALTIME_RECOVERY_LIVE_ONLY
		} else {
			recovery = append(recovery, &realtimev1.RealtimeServerFrame{Frame: &realtimev1.RealtimeServerFrame_Snapshot{
				Snapshot: s.snapshotLocked(userID),
			}})
		}
	}
	caught := cursorPrefix + strconv.Itoa(len(s.events))
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, subscr)
		s.mu.Unlock()
	}()
	for _, f := range recovery {
		if writeFrame(f) != nil {
			return
		}
	}
	if writeFrame(&realtimev1.RealtimeServerFrame{Frame: &realtimev1.RealtimeServerFrame_CaughtUp{CaughtUp: &realtimev1.RealtimeCaughtUp{
		Cursor: caught, Recovery: result,
	}}}) != nil {
		return
	}
	heartbeat := time.NewTicker(s.HeartbeatInterval)
	defer heartbeat.Stop()
	go func() {
		// Drain client frames; the protocol has none after subscribe.
		for {
			if _, _, err := conn.Read(connCtx); err != nil {
				connCancel()
				return
			}
		}
	}()
	for {
		select {
		case <-connCtx.Done():
			return
		case f := <-subscr.out:
			if writeFrame(f) != nil {
				return
			}
			if f.GetClose() != nil {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
		case <-heartbeat.C:
			s.mu.Lock()
			cursor := fmt.Sprintf("%s%d", cursorPrefix, len(s.events))
			s.mu.Unlock()
			if writeFrame(&realtimev1.RealtimeServerFrame{Frame: &realtimev1.RealtimeServerFrame_Heartbeat{
				Heartbeat: &realtimev1.RealtimeHeartbeat{Cursor: &cursor},
			}}) != nil {
				return
			}
		}
	}
}

func (s *Server) snapshotLocked(userID string) *realtimev1.RealtimeSnapshot {
	snap := &realtimev1.RealtimeSnapshot{Server: &apiv1.ServerPublicProfile{Version: Version}}
	seenUsers := map[string]bool{userID: true}
	for _, r := range s.sortedRoomsLocked() {
		if !slices.Contains(r.Members, userID) {
			continue
		}
		snap.Rooms = append(snap.Rooms, s.roomWithViewerLocked(r, userID))
		for _, m := range r.Members {
			seenUsers[m] = true
		}
	}
	for _, gid := range s.groupOrder {
		snap.RoomGroups = append(snap.RoomGroups, s.groupLocked(s.groups[gid], userID))
	}
	for uid := range seenUsers {
		if u, ok := s.users[uid]; ok {
			snap.Users = append(snap.Users, &apiv1.DirectoryMember{User: s.userProtoLocked(u)})
		}
	}
	return snap
}
