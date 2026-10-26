package connector

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	realtimev1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/realtime/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattotest"
)

// Regression tests for issues found in review.

func TestTokenEndpointOutageIsNotBadCredentials(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	login := w.br.GetCachedUserLoginByID(chattoid.MakeUserLoginID(w.alice.ID))
	w.srv.TokenEndpointDown.Store(true)
	w.srv.ExpireAccessTokens()
	w.srv.CloseRealtime(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_AUTHENTICATION_REQUIRED, true, 0)
	w.eventually("transient disconnect", func() bool {
		return w.conn.loginState(login.ID) == status.StateTransientDisconnect
	})
	time.Sleep(200 * time.Millisecond)
	if w.conn.loginState(login.ID) == status.StateBadCredentials {
		t.Fatal("a failing token endpoint was reported as bad credentials")
	}
	w.srv.TokenEndpointDown.Store(false)
	w.eventually("reconnected", func() bool { return w.conn.loginState(login.ID) == status.StateConnected })
	w.srv.PostMessage(w.bob.ID, w.room.ID, "after the outage")
	w.waitMessage(portal.MXID, "after the outage")
}

func TestTransientGetMessageFailureIsRetried(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.FailGetMessage.Store(1)
	w.srv.PostMessage(w.bob.ID, w.room.ID, "eventually delivered")
	w.waitMessage(portal.MXID, "eventually delivered")
}

func TestForwardFillWhenNewestIsThreadReply(t *testing.T) {
	w := newWorld(t, withBackfill(50))
	portal := w.portal(w.room.ID)
	root := w.srv.PostMessage(w.bob.ID, w.room.ID, "old root")
	w.waitMessage(portal.MXID, "old root")
	w.srv.PostMessage(w.bob.ID, w.room.ID, "reply before outage", chattotest.InThread(root.ID))
	w.waitMessage(portal.MXID, "reply before outage")

	w.srv.ExpireCursors()
	w.srv.DropRealtime()
	time.Sleep(10 * time.Millisecond)
	w.srv.PostMessage(w.bob.ID, w.room.ID, "new root during outage")
	w.srv.PostMessage(w.bob.ID, w.room.ID, "reply during outage", chattotest.InThread(root.ID))

	w.waitMessage(portal.MXID, "new root during outage")
	reply := w.waitMessage(portal.MXID, "reply during outage")
	if rel := reply.Message().RelatesTo; rel == nil || rel.Type != event.RelThread {
		t.Fatalf("forward-filled reply lost its thread: %+v", rel)
	}
}

func TestCaptionedMatrixMediaSurvivesChattoEdit(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	uri, _, _ := w.mx.BotIntent().UploadMedia(w.ctx, "", []byte("img"), "pic.png", "image/png")
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgImage, Body: "my caption", FileName: "pic.png", URL: uri,
		Info: &event.FileInfo{MimeType: "image/png", Size: 3},
	})
	sent := w.waitChatto(t, func(m *chattotest.Message) bool { return m.Body == "my caption" })
	w.eventually("db part", func() bool { return len(w.dbMessage(sent.ID)) == 1 })
	before := len(w.messagesIn(portal.MXID))
	w.srv.EditMessage(sent.ID, "changed on chatto")
	time.Sleep(400 * time.Millisecond)
	if after := len(w.messagesIn(portal.MXID)); after != before {
		for _, e := range w.messagesIn(portal.MXID)[before:] {
			t.Logf("unexpected event: %+v", e.Message())
		}
		t.Fatalf("chatto edit of captioned media produced %d Matrix events", after-before)
	}
}

func TestAssetProcessingDoesNotEditText(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "video below", w.srv.WithAttachment("v.mp4", "video/mp4", []byte("mp4"), true))
	textEvt := w.waitMessage(portal.MXID, "video below")
	placeholder := w.waitMessage(portal.MXID, "still processing")
	w.srv.FinishProcessing(msg.ID, msg.Attachments[0].ID)
	w.eventually("video edit", func() bool {
		for _, e := range w.messagesIn(portal.MXID) {
			if m := e.Message(); m != nil && m.RelatesTo != nil && m.RelatesTo.EventID == placeholder.ID {
				return true
			}
		}
		return false
	})
	for _, e := range w.messagesIn(portal.MXID) {
		if m := e.Message(); m != nil && m.RelatesTo != nil && m.RelatesTo.Type == event.RelReplace && m.RelatesTo.EventID == textEvt.ID {
			t.Fatal("asset processing produced a no-op text edit")
		}
	}
}

func TestOwnEditEchoIsNotBridgedBack(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	orig := w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "draft"})
	sent := w.waitChatto(t, func(m *chattotest.Message) bool { return m.Body == "draft" })
	w.eventually("db part", func() bool { return len(w.dbMessage(sent.ID)) == 1 })
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText, Body: "* final",
		NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: "final"},
		RelatesTo:  (&event.RelatesTo{}).SetReplace(orig),
	})
	w.eventually("edit on chatto", func() bool { return w.srv.Message(sent.ID).Body == "final" })
	time.Sleep(400 * time.Millisecond)
	if n := len(w.messagesIn(portal.MXID)); n != 0 {
		t.Fatalf("own edit echoed back as %d Matrix events", n)
	}
}

func TestReadReceiptOnNoticeMarksRoomRead(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "real message")
	w.waitMessage(portal.MXID, "real message")
	w.srv.Publish(w.room.ID, w.bob.ID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_VoiceCallStarted{
		VoiceCallStarted: &realtimev1.VoiceCallStartedEvent{RoomId: w.room.ID, CallId: "C1"},
	}})
	notice := w.waitMessage(portal.MXID, "Started a call")
	w.sendMatrix(portal.MXID, aliceMXID, event.EphemeralEventReceipt, &event.ReceiptEventContent{
		notice.ID: {event.ReceiptTypeRead: {aliceMXID: {Timestamp: time.Now()}}},
	})
	w.eventually("room read through latest message", func() bool { return w.srv.ReadMarker(w.alice.ID, w.room.ID) == msg.ID })
}
