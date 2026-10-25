package chattoclient_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	realtimev1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/realtime/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattotest"
)

// recorder is a RealtimeHandler that records everything it gets.
type recorder struct {
	mu        sync.Mutex
	events    []*realtimev1.RealtimeEvent
	snapshots int
	caughtUp  []realtimev1.RealtimeRecovery
	states    []chattoclient.RealtimeState
	cursor    string
	notify    chan struct{}
}

func newRecorder() *recorder { return &recorder{notify: make(chan struct{}, 1000)} }

func (r *recorder) poke() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *recorder) HandleEvent(_ context.Context, evt *realtimev1.RealtimeEvent) {
	r.mu.Lock()
	r.events = append(r.events, evt)
	r.mu.Unlock()
	r.poke()
}

func (r *recorder) HandleSnapshot(context.Context, *realtimev1.RealtimeSnapshot) {
	r.mu.Lock()
	r.snapshots++
	r.mu.Unlock()
	r.poke()
}

func (r *recorder) HandleCaughtUp(_ context.Context, rec realtimev1.RealtimeRecovery) {
	r.mu.Lock()
	r.caughtUp = append(r.caughtUp, rec)
	r.mu.Unlock()
	r.poke()
}

func (r *recorder) HandleState(s chattoclient.RealtimeState, _ error) {
	r.mu.Lock()
	r.states = append(r.states, s)
	r.mu.Unlock()
	r.poke()
}

func (r *recorder) SaveCursor(c string) {
	r.mu.Lock()
	r.cursor = c
	r.mu.Unlock()
}

// waitFor polls cond until it holds or the timeout expires.
func (r *recorder) waitFor(t *testing.T, what string, cond func(r *recorder) bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		r.mu.Lock()
		ok := cond(r)
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-r.notify:
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func messageIDs(events []*realtimev1.RealtimeEvent) []string {
	var ids []string
	for _, e := range events {
		if e.GetMessagePosted() != nil {
			ids = append(ids, e.GetId())
		}
	}
	return ids
}

type rtFixture struct {
	srv   *chattotest.Server
	alice *chattotest.User
	bob   *chattotest.User
	room  *chattotest.Room
	rec   *recorder
	rt    *chattoclient.Realtime
	errc  chan error
	stop  context.CancelFunc
	src   *chattoclient.SessionTokenSource
}

func newRTFixture(t *testing.T) *rtFixture {
	t.Helper()
	srv := chattotest.New()
	alice := srv.AddUser("alice", "Alice", "pw")
	bob := srv.AddUser("bob", "Bob", "pw")
	room := srv.AddChannel("general", "", alice.ID, bob.ID)
	access, refresh := srv.IssueSession(alice.ID)
	base, _ := chattoclient.New(srv.URL(), chattoclient.Options{})
	src := chattoclient.NewSessionTokenSource(base, chattoclient.Session{
		AccessToken: access, RefreshToken: refresh, AccessExpiry: time.Now().Add(time.Hour),
	}, nil)
	f := &rtFixture{srv: srv, alice: alice, bob: bob, room: room, rec: newRecorder(), errc: make(chan error, 1), src: src}
	f.rt = chattoclient.NewRealtime(base.WithTokens(src), f.rec, "", "test", zerolog.Nop())
	f.rt.MinBackoff = 10 * time.Millisecond
	f.rt.MaxBackoff = 50 * time.Millisecond
	t.Cleanup(func() {
		if f.stop != nil {
			f.stop()
		}
		srv.Close()
	})
	return f
}

func (f *rtFixture) start() {
	ctx, cancel := context.WithCancel(context.Background())
	f.stop = cancel
	go func() { f.errc <- f.rt.Run(ctx) }()
}

func TestRealtimeInitialSnapshotAndLiveEvents(t *testing.T) {
	f := newRTFixture(t)
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	if f.rec.snapshots != 1 || f.rec.caughtUp[0] != realtimev1.RealtimeRecovery_REALTIME_RECOVERY_SNAPSHOT {
		t.Fatalf("expected snapshot recovery, got snapshots=%d recovery=%v", f.rec.snapshots, f.rec.caughtUp)
	}
	m := f.srv.PostMessage(f.bob.ID, f.room.ID, "hello")
	f.rec.waitFor(t, "message", func(r *recorder) bool { return len(messageIDs(r.events)) == 1 })
	if got := messageIDs(f.rec.events)[0]; got != m.ID {
		t.Fatalf("realtime id %q != message id %q", got, m.ID)
	}
	if f.rt.Cursor() == "" {
		t.Fatal("cursor not advanced")
	}
}

func TestRealtimeResumeAfterDrop(t *testing.T) {
	f := newRTFixture(t)
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	first := f.srv.PostMessage(f.bob.ID, f.room.ID, "one")
	f.rec.waitFor(t, "first message", func(r *recorder) bool { return len(messageIDs(r.events)) == 1 })

	f.srv.DropRealtime()
	// Messages sent while disconnected must be replayed exactly once.
	missed := f.srv.PostMessage(f.bob.ID, f.room.ID, "two")
	f.rec.waitFor(t, "resume", func(r *recorder) bool { return len(r.caughtUp) == 2 })
	if f.rec.caughtUp[1] != realtimev1.RealtimeRecovery_REALTIME_RECOVERY_RESUMED {
		t.Fatalf("expected resumed, got %v", f.rec.caughtUp[1])
	}
	f.rec.waitFor(t, "missed message", func(r *recorder) bool { return len(messageIDs(r.events)) == 2 })
	ids := messageIDs(f.rec.events)
	if ids[0] != first.ID || ids[1] != missed.ID {
		t.Fatalf("unexpected events %v", ids)
	}
	if f.rec.snapshots != 1 {
		t.Fatalf("resume must not send a snapshot, got %d", f.rec.snapshots)
	}
}

func TestRealtimeExpiredCursorFallsBackToSnapshot(t *testing.T) {
	f := newRTFixture(t)
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	f.srv.PostMessage(f.bob.ID, f.room.ID, "one")
	f.rec.waitFor(t, "message", func(r *recorder) bool { return len(messageIDs(r.events)) == 1 })
	f.srv.ExpireCursors()
	f.srv.DropRealtime()
	f.rec.waitFor(t, "second caught up", func(r *recorder) bool { return len(r.caughtUp) == 2 })
	if f.rec.caughtUp[1] != realtimev1.RealtimeRecovery_REALTIME_RECOVERY_SNAPSHOT || f.rec.snapshots != 2 {
		t.Fatalf("expected snapshot fallback, got %v / %d", f.rec.caughtUp, f.rec.snapshots)
	}
}

func TestRealtimeResyncRequiredDropsCursor(t *testing.T) {
	f := newRTFixture(t)
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	f.srv.PostMessage(f.bob.ID, f.room.ID, "one")
	f.rec.waitFor(t, "message", func(r *recorder) bool { return len(messageIDs(r.events)) == 1 })
	f.srv.CloseRealtime(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_RESYNC_REQUIRED, true, 0)
	f.rec.waitFor(t, "second caught up", func(r *recorder) bool { return len(r.caughtUp) == 2 })
	if f.rec.caughtUp[1] != realtimev1.RealtimeRecovery_REALTIME_RECOVERY_SNAPSHOT {
		t.Fatalf("resync must not resume, got %v", f.rec.caughtUp[1])
	}
}

func TestRealtimeAuthRequiredRefreshesAndReconnects(t *testing.T) {
	f := newRTFixture(t)
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	before := f.src.Session().AccessToken
	f.srv.ExpireAccessTokens()
	f.srv.CloseRealtime(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_AUTHENTICATION_REQUIRED, true, 0)
	f.rec.waitFor(t, "reconnect", func(r *recorder) bool { return len(r.caughtUp) == 2 })
	if f.src.Session().AccessToken == before {
		t.Fatal("access token was not refreshed before reconnecting")
	}
}

func TestRealtimeSessionTerminatedIsFatal(t *testing.T) {
	f := newRTFixture(t)
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	f.srv.CloseRealtime(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_SESSION_TERMINATED, false, 0)
	select {
	case err := <-f.errc:
		if !errors.Is(err, chattoclient.ErrRealtimeFatal) {
			t.Fatalf("expected fatal error, got %v", err)
		}
		var closeErr *chattoclient.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_SESSION_TERMINATED {
			t.Fatalf("expected close error, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	if f.rec.states[len(f.rec.states)-1] != chattoclient.RealtimeBadCredentials {
		t.Fatalf("last state %v", f.rec.states)
	}
}

func TestRealtimeRetryAfterIsHonoured(t *testing.T) {
	f := newRTFixture(t)
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	start := time.Now()
	f.srv.CloseRealtime(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_TEMPORARILY_UNAVAILABLE, true, 400*time.Millisecond)
	f.rec.waitFor(t, "reconnect", func(r *recorder) bool { return len(r.caughtUp) == 2 })
	// Full jitter waits at least half of retry_after.
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("reconnected after %v, before retry_after", elapsed)
	}
}

func TestRealtimeHeartbeatTimeoutReconnects(t *testing.T) {
	f := newRTFixture(t)
	f.rt.HeartbeatTimeout = 150 * time.Millisecond
	f.start()
	f.rec.waitFor(t, "two connections", func(r *recorder) bool { return len(r.caughtUp) >= 2 })
}

func TestRealtimeHeartbeatAdvancesCursor(t *testing.T) {
	f := newRTFixture(t)
	f.srv.HeartbeatInterval = 30 * time.Millisecond
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	f.srv.Publish("", "", &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_ServerProfileChanged{ServerProfileChanged: &realtimev1.ServerProfileChangedEvent{}}})
	f.rec.waitFor(t, "saved cursor", func(r *recorder) bool { return r.cursor == "c1" })
}

func TestRealtimeStopsOnContextCancel(t *testing.T) {
	f := newRTFixture(t)
	f.start()
	f.rec.waitFor(t, "caught up", func(r *recorder) bool { return len(r.caughtUp) == 1 })
	f.stop()
	select {
	case err := <-f.errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestRealtimeURL(t *testing.T) {
	c, _ := chattoclient.New("https://chat.example.com", chattoclient.Options{})
	rt := chattoclient.NewRealtime(c, newRecorder(), "", "x", zerolog.Nop())
	if rt.URL() != "wss://chat.example.com/api/realtime" {
		t.Fatalf("URL = %q", rt.URL())
	}
	c, _ = chattoclient.New("http://localhost:4000", chattoclient.Options{})
	rt = chattoclient.NewRealtime(c, newRecorder(), "", "x", zerolog.Nop())
	if rt.URL() != "ws://localhost:4000/api/realtime" {
		t.Fatalf("URL = %q", rt.URL())
	}
}

func TestEventType(t *testing.T) {
	evt := &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_ReactionAdded{ReactionAdded: &realtimev1.ReactionAddedEvent{}}}
	if got := chattoclient.EventType(evt); got != "reaction_added" {
		t.Fatalf("EventType = %q", got)
	}
	if got := chattoclient.EventType(&realtimev1.RealtimeEvent{}); got != "unknown" {
		t.Fatalf("empty EventType = %q", got)
	}
}
