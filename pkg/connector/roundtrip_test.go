package connector

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattotest"
)

const aliceMXID = id.UserID("@alice:test")

type world struct {
	*harness
	alice, bob *chattotest.User
	room       *chattotest.Room
}

// newWorld starts a harness with alice (logged in) and bob in #general.
func newWorld(t *testing.T, opts ...harnessOpt) *world {
	h := newHarness(t, opts...)
	alice := h.srv.AddUser("alice", "Alice", "pw")
	bob := h.srv.AddUser("bob", "Bob Builder", "pw")
	room := h.srv.AddChannel("general", "", alice.ID, bob.ID)
	h.login(aliceMXID, "alice", "pw")
	return &world{harness: h, alice: alice, bob: bob, room: room}
}

func TestChattoMessageReachesMatrix(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	if portal.Name != "general" {
		t.Errorf("portal name %q", portal.Name)
	}
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "hello **world** @alice")
	evt := w.waitMessage(portal.MXID, "hello")
	content := evt.Message()
	if evt.Sender != w.mx.FormatGhostMXID(chattoid.MakeUserID(testServer, w.bob.ID)) {
		t.Errorf("sender %s is not bob's ghost", evt.Sender)
	}
	if content.Format != event.FormatHTML || content.FormattedBody == "" {
		t.Fatalf("expected formatted body, got %+v", content)
	}
	if want := `<strong>world</strong>`; !strings.Contains(content.FormattedBody, want) {
		t.Errorf("formatted body %q lacks %q", content.FormattedBody, want)
	}
	// alice is logged in, so the mention points at her real Matrix account.
	if content.Mentions == nil || len(content.Mentions.UserIDs) != 1 || content.Mentions.UserIDs[0] != aliceMXID {
		t.Errorf("mentions = %+v", content.Mentions)
	}
	parts := w.dbMessage(msg.ID)
	if len(parts) != 1 || parts[0].MXID != evt.ID {
		t.Fatalf("db parts %+v", parts)
	}
}

func TestMatrixMessageReachesChatto(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	bobGhost := w.mx.FormatGhostMXID(chattoid.MakeUserID(testServer, w.bob.ID))
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "hi Bob",
		Format:        event.FormatHTML,
		FormattedBody: `hi <a href="https://matrix.to/#/` + string(bobGhost) + `">Bob</a>, <em>nice</em>`,
	})
	var sent *chattotest.Message
	w.eventually("message on chatto", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.ActorID == w.alice.ID {
				sent = m
				return true
			}
		}
		return false
	})
	if sent.Body != "hi @bob, *nice*" {
		t.Fatalf("chatto body %q", sent.Body)
	}
	// The realtime echo must not come back to Matrix as a second message.
	time.Sleep(300 * time.Millisecond)
	for _, e := range w.messagesIn(portal.MXID) {
		if e.Sender == aliceMXID || e.Sender == w.mx.FormatGhostMXID(chattoid.MakeUserID(testServer, w.alice.ID)) {
			t.Fatalf("echo was bridged back: %+v", e.Message())
		}
	}
}

func TestEchoRaceIsSuppressed(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	// Chatto publishes the realtime event well before CreateMessage returns.
	w.srv.PublishBeforeResponse = 300 * time.Millisecond
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "racy"})
	w.eventually("message on chatto", func() bool { return len(w.srv.Messages(w.room.ID)) == 1 })
	time.Sleep(600 * time.Millisecond)
	if n := len(w.messagesIn(portal.MXID)); n != 0 {
		t.Fatalf("echo bridged back %d times", n)
	}
	// A message from alice's other Chatto client is not an echo.
	w.srv.PublishBeforeResponse = 0
	w.srv.PostMessage(w.alice.ID, w.room.ID, "from the web app")
	w.waitMessage(portal.MXID, "from the web app")
}

func TestIdempotentRetries(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	evtID := w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "once"})
	w.eventually("message on chatto", func() bool { return len(w.srv.Messages(w.room.ID)) == 1 })
	// A redelivered Matrix event maps to the same idempotency key.
	again := evtID
	if idempotencyKey(evtID) != idempotencyKey(again) || idempotencyKey(evtID) == idempotencyKey("$other") {
		t.Fatal("idempotency keys are not deterministic per event")
	}
}

func TestEditsBothWays(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "first version")
	orig := w.waitMessage(portal.MXID, "first version")
	w.srv.EditMessage(msg.ID, "second version")
	edit := w.waitMessage(portal.MXID, "second version")
	content := edit.Message()
	if content.RelatesTo == nil || content.RelatesTo.Type != event.RelReplace || content.RelatesTo.EventID != orig.ID {
		t.Fatalf("edit does not replace original: %+v", content.RelatesTo)
	}
	if content.NewContent == nil || content.NewContent.Body != "second version" {
		t.Fatalf("new content %+v", content.NewContent)
	}

	// Matrix → Chatto edit.
	mxEvt := w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "typo"})
	var sent *chattotest.Message
	w.eventually("alice message", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.ActorID == w.alice.ID {
				sent = m
				return true
			}
		}
		return false
	})
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType:    event.MsgText,
		Body:       "* fixed",
		NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: "fixed"},
		RelatesTo:  (&event.RelatesTo{}).SetReplace(mxEvt),
	})
	w.eventually("edit on chatto", func() bool { return w.srv.Message(sent.ID).Body == "fixed" })
}

func TestDeletesBothWays(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "delete me")
	orig := w.waitMessage(portal.MXID, "delete me")
	w.srv.RetractMessage(msg.ID)
	w.eventually("redaction", func() bool {
		return len(w.mx.Events(func(e *sentEvent) bool {
			if e.Type != event.EventRedaction {
				return false
			}
			_ = e.Content.ParseRaw(e.Type)
			r, _ := e.Content.Parsed.(*event.RedactionEventContent)
			return r != nil && r.Redacts == orig.ID
		})) == 1
	})

	mxEvt := w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "regret"})
	var sent *chattotest.Message
	w.eventually("alice message", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.ActorID == w.alice.ID {
				sent = m
				return true
			}
		}
		return false
	})
	w.eventually("db entry", func() bool { return len(w.dbMessage(sent.ID)) == 1 })
	w.sendMatrix(portal.MXID, aliceMXID, event.EventRedaction, &event.RedactionEventContent{Redacts: mxEvt})
	w.eventually("retraction on chatto", func() bool { return !w.srv.Message(sent.ID).DeletedAt.IsZero() })
}

func TestReactionsBothWays(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "react to me")
	orig := w.waitMessage(portal.MXID, "react to me")

	w.srv.React(w.bob.ID, msg.ID, "tada")
	w.eventually("reaction on matrix", func() bool {
		return len(w.mx.Events(func(e *sentEvent) bool {
			r := e.Reaction()
			return e.Type == event.EventReaction && r != nil && r.RelatesTo.Key == "🎉" && r.RelatesTo.EventID == orig.ID
		})) == 1
	})

	w.sendMatrix(portal.MXID, aliceMXID, event.EventReaction, &event.ReactionEventContent{
		RelatesTo: event.RelatesTo{Type: event.RelAnnotation, EventID: orig.ID, Key: "👍🏽"},
	})
	w.eventually("reaction on chatto", func() bool {
		m := w.srv.Message(msg.ID)
		return len(m.Reactions["+1"]) == 1 && m.Reactions["+1"][0] == w.alice.ID
	})
}

func TestThreadsBothWays(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	root := w.srv.PostMessage(w.bob.ID, w.room.ID, "thread root")
	rootEvt := w.waitMessage(portal.MXID, "thread root")
	w.srv.PostMessage(w.bob.ID, w.room.ID, "in the thread", chattotest.InThread(root.ID))
	reply := w.waitMessage(portal.MXID, "in the thread")
	rel := reply.Message().RelatesTo
	if rel == nil || rel.Type != event.RelThread || rel.EventID != rootEvt.ID {
		t.Fatalf("thread reply not related to root: %+v", rel)
	}

	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType:   event.MsgText,
		Body:      "matrix thread reply",
		RelatesTo: (&event.RelatesTo{}).SetThread(rootEvt.ID, rootEvt.ID),
	})
	w.eventually("thread reply on chatto", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.Body == "matrix thread reply" {
				return m.ThreadRoot == root.ID
			}
		}
		return false
	})
}

func TestRepliesBothWays(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	target := w.srv.PostMessage(w.bob.ID, w.room.ID, "original")
	targetEvt := w.waitMessage(portal.MXID, "original")
	w.srv.PostMessage(w.bob.ID, w.room.ID, "a reply", chattotest.ReplyTo(target.ID))
	reply := w.waitMessage(portal.MXID, "a reply")
	if r := reply.Message().RelatesTo; r == nil || r.GetReplyTo() != targetEvt.ID {
		t.Fatalf("reply relation %+v", r)
	}
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType:   event.MsgText,
		Body:      "matrix reply",
		RelatesTo: (&event.RelatesTo{}).SetReplyTo(targetEvt.ID),
	})
	w.eventually("reply on chatto", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.Body == "matrix reply" {
				return m.InReplyTo == target.ID
			}
		}
		return false
	})
}
