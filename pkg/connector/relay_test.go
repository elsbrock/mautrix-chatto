package connector

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattotest"
)

const carolMXID = id.UserID("@carol:test")

func withGhostBots() harnessOpt {
	return func(_ *bridgeconfig.BridgeConfig, nc *Config) {
		nc.GhostBots.Enabled = true
		nc.GhostBots.LoginPrefix = "mx-"
	}
}

// setRelay makes alice's login the relay of the room's portal and puts carol
// (who has no Chatto login) into the Matrix room.
func (w *world) setRelay(t *testing.T) id.RoomID {
	t.Helper()
	dbPortal := w.portal(w.room.ID)
	portal, err := w.br.GetPortalByKey(w.ctx, chattoid.MakePortalKey(w.room.ID))
	if err != nil {
		t.Fatal(err)
	}
	login := w.br.GetCachedUserLoginByID(chattoid.MakeUserLoginID(w.alice.ID))
	if err := portal.SetRelay(w.ctx, login); err != nil {
		t.Fatal(err)
	}
	w.mx.mu.Lock()
	w.mx.rooms[dbPortal.MXID].members[carolMXID] = event.MembershipJoin
	w.mx.mu.Unlock()
	return dbPortal.MXID
}

func (w *world) waitChatto(t *testing.T, cond func(*chattotest.Message) bool) *chattotest.Message {
	t.Helper()
	var found *chattotest.Message
	w.eventually("message on chatto", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if cond(m) {
				found = m
				return true
			}
		}
		return false
	})
	return found
}

func TestRelayModeWithNamePrefix(t *testing.T) {
	w := newWorld(t)
	roomID := w.setRelay(t)
	w.sendMatrix(roomID, carolMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "hello from matrix"})
	msg := w.waitChatto(t, func(m *chattotest.Message) bool { return strings.Contains(m.Body, "hello from matrix") })
	if msg.ActorID != w.alice.ID || !strings.Contains(msg.Body, "carol") {
		t.Fatalf("relayed message %q by %s", msg.Body, msg.ActorID)
	}
}

func TestGhostBotsPostAsDedicatedAccounts(t *testing.T) {
	w := newWorld(t, withGhostBots())
	roomID := w.setRelay(t)
	evtID := w.sendMatrix(roomID, carolMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "hi, I'm carol"})
	msg := w.waitChatto(t, func(m *chattotest.Message) bool { return m.Body == "hi, I'm carol" })
	bots := w.srv.Bots()
	if len(bots) != 1 || msg.ActorID != bots[0].ID || bots[0].BotOwner != w.alice.ID {
		t.Fatalf("message by %s, bots %+v", msg.ActorID, bots)
	}
	if !strings.HasPrefix(bots[0].Login, "mx-carol-") {
		t.Errorf("bot login %q", bots[0].Login)
	}
	if n := len(w.srv.PermissionsFor(bots[0].ID)); n != len(BotPermissions) {
		t.Errorf("bot got %d permission grants", n)
	}
	// The bot's message must not come back to Matrix.
	time.Sleep(300 * time.Millisecond)
	for _, e := range w.messagesIn(roomID) {
		if m := e.Message(); m != nil && strings.Contains(m.Body, "I'm carol") {
			t.Fatalf("ghost bot echo bridged back from %s", e.Sender)
		}
	}
	// Edits go through the same bot.
	w.sendMatrix(roomID, carolMXID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText, Body: "* edited",
		NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: "hi, I'm carol (edited)"},
		RelatesTo:  (&event.RelatesTo{}).SetReplace(evtID),
	})
	w.eventually("bot edit", func() bool { return w.srv.Message(msg.ID).Body == "hi, I'm carol (edited)" })
	// A second message reuses the bot.
	w.sendMatrix(roomID, carolMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "again"})
	w.waitChatto(t, func(m *chattotest.Message) bool { return m.Body == "again" })
	if len(w.srv.Bots()) != 1 {
		t.Fatalf("second message created another bot")
	}
	// Ghost bots never get Matrix ghosts of their own.
	login := w.br.GetCachedUserLoginByID(chattoid.MakeUserLoginID(w.alice.ID))
	contacts, err := login.Client.(*ChattoClient).GetContactList(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range contacts {
		if c.UserID == chattoid.MakeUserID(bots[0].ID) {
			t.Fatal("ghost bot appears in the contact list")
		}
	}
}

func TestGhostBotLogin(t *testing.T) {
	tests := map[id.UserID]string{
		"@carol:example.com":                          "mx-carol-",
		"@Some.User:example.com":                      "mx-some.user-",
		"@_weird=chars/here:example.com":              "mx-_weirdcharshere-",
		"@averyveryverylongusernamethatgoesonandon:x": "mx-averyveryverylongusern",
	}
	for mxid, prefix := range tests {
		got := ghostBotLogin("mx-", mxid)
		if !strings.HasPrefix(got, prefix) || len(got) > 32 || len(got) < 2 {
			t.Errorf("ghostBotLogin(%s) = %q, want prefix %q and length ≤ 32", mxid, got, prefix)
		}
		if again := ghostBotLogin("mx-", mxid); again != got {
			t.Errorf("not deterministic: %q vs %q", got, again)
		}
	}
	if ghostBotLogin("mx-", "@a:x") == ghostBotLogin("mx-", "@a:y") {
		t.Error("different users must not share a login")
	}
}
