package connector

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattotest"
)

// harness wires a real bridgev2 Bridge to a fake Chatto server and a mock
// homeserver.
type harness struct {
	t    *testing.T
	ctx  context.Context
	srv  *chattotest.Server
	mx   *mockMatrix
	br   *bridgev2.Bridge
	conn *Connector
}

type harnessOpt func(cfg *bridgeconfig.BridgeConfig, nc *Config)

func withBackfill(n int) harnessOpt {
	return func(cfg *bridgeconfig.BridgeConfig, _ *Config) {
		cfg.Backfill.Enabled = true
		cfg.Backfill.MaxInitialMessages = n
		cfg.Backfill.MaxCatchupMessages = n
		cfg.Backfill.Threads.MaxInitialMessages = n
	}
}

var harnessCounter int

func newHarness(t *testing.T, opts ...harnessOpt) *harness {
	t.Helper()
	srv := chattotest.New()
	t.Cleanup(srv.Close)

	harnessCounter++
	dsn := fmt.Sprintf("file:harness%d?mode=memory&cache=shared&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate", harnessCounter)
	raw, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := &bridgeconfig.BridgeConfig{
		CommandPrefix:         "!chatto",
		PrivateChatPortalMeta: true,
		PortalEventBuffer:     64,
		Permissions: bridgeconfig.PermissionConfig{
			"*": &bridgeconfig.Permissions{SendEvents: true, Commands: true, Login: true, ManageRelay: true},
		},
	}
	conn := &Connector{HTTPClient: httpClientNoRedirectLimit, Version: "test"}
	conn.Config = Config{
		ServerURL:          srv.URL(),
		SyncPresence:       true,
		CallNotices:        true,
		ThreadEcho:         "skip",
		AllowPasswordLogin: true,
	}
	for _, o := range opts {
		o(cfg, &conn.Config)
	}
	if err := conn.Config.PostProcess(); err != nil {
		t.Fatal(err)
	}
	mx := newMockMatrix()
	// Bridge goroutines may still log after a test ends, which t.Log does
	// not allow, so logs go to stderr (verbose) or nowhere.
	log := zerolog.Nop()
	if testing.Verbose() {
		log = zerolog.New(os.Stderr).With().Str("test", t.Name()).Logger().Level(zerolog.DebugLevel)
	}
	br := bridgev2.NewBridge("chatto", db, log, cfg, mx, conn, commands.NewProcessor)
	ctx, cancel := context.WithCancel(log.WithContext(context.Background()))
	if err := br.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		br.StopWithTimeout(5 * time.Second)
		cancel()
	})
	return &harness{t: t, ctx: ctx, srv: srv, mx: mx, br: br, conn: conn}
}

// login logs mxid into Chatto with a password and waits for the realtime
// subscription to be up.
func (h *harness) login(mxid id.UserID, chattoLogin, password string) *bridgev2.UserLogin {
	h.t.Helper()
	user, err := h.br.GetUserByMXID(h.ctx, mxid)
	if err != nil {
		h.t.Fatal(err)
	}
	before := h.srv.Subscribers()
	proc, err := h.conn.CreateLogin(h.ctx, user, LoginFlowPassword)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := proc.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	step, err := proc.(bridgev2.LoginProcessUserInput).SubmitUserInput(h.ctx, map[string]string{"login": chattoLogin, "password": password})
	if err != nil {
		h.t.Fatalf("login failed: %v", err)
	}
	if step.Type != bridgev2.LoginStepTypeComplete {
		h.t.Fatalf("unexpected step %v", step.Type)
	}
	if !h.srv.WaitForSubscribers(before+1, 5*time.Second) {
		h.t.Fatal("realtime subscription did not start")
	}
	return step.CompleteParams.UserLogin
}

// eventually polls cond until it returns true.
func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case <-h.mx.notify:
		case <-time.After(20 * time.Millisecond):
		}
	}
	h.t.Fatalf("timed out waiting for %s", what)
}

// portal waits until the portal of a Chatto room has a Matrix room. It reads
// the database row so that it never races with the portal's event loop.
func (h *harness) portal(roomID string) *database.Portal {
	h.t.Helper()
	return h.portalByID(chattoid.MakePortalKey(roomID))
}

func (h *harness) portalByID(key networkid.PortalKey) *database.Portal {
	h.t.Helper()
	var portal *database.Portal
	h.eventually("portal for "+string(key.ID), func() bool {
		p, err := h.br.DB.Portal.GetByKey(h.ctx, key)
		if err == nil && p != nil && p.MXID != "" {
			portal = p
			return true
		}
		return false
	})
	return portal
}

// messagesIn returns the m.room.message events sent into a Matrix room.
func (h *harness) messagesIn(roomID id.RoomID) []*sentEvent {
	return h.mx.Events(func(e *sentEvent) bool {
		return e.RoomID == roomID && (e.Type == event.EventMessage || e.Type == event.EventSticker)
	})
}

// waitMessage waits for a message event in roomID whose body contains text.
func (h *harness) waitMessage(roomID id.RoomID, text string) *sentEvent {
	h.t.Helper()
	var found *sentEvent
	h.eventually(fmt.Sprintf("message %q", text), func() bool {
		for _, e := range h.messagesIn(roomID) {
			if msg := e.Message(); msg != nil && strings.Contains(msg.Body, text) {
				found = e
				return true
			}
		}
		return false
	})
	return found
}

var matrixEventCounter int

// sendMatrix delivers a Matrix event from sender into a portal room.
func (h *harness) sendMatrix(roomID id.RoomID, sender id.UserID, evtType event.Type, content any) id.EventID {
	h.t.Helper()
	matrixEventCounter++
	evtID := id.EventID(fmt.Sprintf("$mx%d:test", matrixEventCounter))
	evt := &event.Event{
		ID:        evtID,
		RoomID:    roomID,
		Sender:    sender,
		Type:      evtType,
		Timestamp: time.Now().UnixMilli(),
		Content:   event.Content{Parsed: content},
	}
	switch evtType.Class {
	case event.UnknownEventType:
		evt.Type.Class = event.MessageEventType
	case event.EphemeralEventType:
		evt.Mautrix.EventSource = event.SourceEphemeral
		evt.Sender = ""
	case event.StateEventType:
		empty := ""
		evt.StateKey = &empty
	}
	h.br.QueueMatrixEvent(h.ctx, evt)
	return evtID
}

// dbMessage returns the bridged message parts for a Chatto message ID.
func (h *harness) dbMessage(chattoID string) []*database.Message {
	parts, err := h.br.DB.Message.GetAllPartsByID(h.ctx, "", chattoid.MakeMessageID(chattoID))
	if err != nil {
		h.t.Fatal(err)
	}
	return parts
}
