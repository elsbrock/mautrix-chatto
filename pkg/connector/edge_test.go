package connector

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
	realtimev1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/realtime/v1"
)

func TestArchivedRoomRefusesSends(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.ArchiveRoom(w.room.ID)
	w.waitMessage(portal.MXID, "archived on Chatto")
	w.eventually("archived metadata", func() bool {
		p, _ := w.br.DB.Portal.GetByKey(w.ctx, chattoid.MakePortalKey(testServer, w.room.ID))
		return p.Metadata.(*chattoid.PortalMetadata).Archived
	})
	before := w.srv.CallCount("CreateMessage")
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "into the void"})
	time.Sleep(300 * time.Millisecond)
	if w.srv.CallCount("CreateMessage") != before {
		t.Fatal("message was sent to an archived room")
	}
}

func TestThreadsDisabledBecomeReplies(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	root := w.srv.PostMessage(w.bob.ID, w.room.ID, "root message")
	rootEvt := w.waitMessage(portal.MXID, "root message")
	w.srv.SetThreadingMode(w.room.ID, apiv1.RoomThreadingMode_ROOM_THREADING_MODE_DISABLED)
	w.eventually("threading mode metadata", func() bool {
		p, _ := w.br.DB.Portal.GetByKey(w.ctx, chattoid.MakePortalKey(testServer, w.room.ID))
		return p.Metadata.(*chattoid.PortalMetadata).ThreadingMode == int32(apiv1.RoomThreadingMode_ROOM_THREADING_MODE_DISABLED)
	})
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText, Body: "thread attempt",
		RelatesTo: (&event.RelatesTo{}).SetThread(rootEvt.ID, rootEvt.ID),
	})
	w.eventually("reply on chatto", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.Body == "thread attempt" {
				return m.ThreadRoot == "" && m.InReplyTo == root.ID
			}
		}
		return false
	})
}

func TestRequiredThreadingPutsRepliesInThread(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.SetThreadingMode(w.room.ID, apiv1.RoomThreadingMode_ROOM_THREADING_MODE_REQUIRED)
	root := w.srv.PostMessage(w.bob.ID, w.room.ID, "required root")
	rootEvt := w.waitMessage(portal.MXID, "required root")
	w.eventually("threading mode metadata", func() bool {
		p, _ := w.br.DB.Portal.GetByKey(w.ctx, chattoid.MakePortalKey(testServer, w.room.ID))
		return p.Metadata.(*chattoid.PortalMetadata).ThreadingMode == int32(apiv1.RoomThreadingMode_ROOM_THREADING_MODE_REQUIRED)
	})
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText, Body: "plain reply", RelatesTo: (&event.RelatesTo{}).SetReplyTo(rootEvt.ID),
	})
	w.eventually("threaded reply on chatto", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.Body == "plain reply" {
				return m.ThreadRoot == root.ID
			}
		}
		return false
	})
}

func TestRoomDeletedRemovesPortal(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.DeleteRoom(w.room.ID)
	w.eventually("matrix room deleted", func() bool {
		w.mx.mu.Lock()
		defer w.mx.mu.Unlock()
		_, exists := w.mx.rooms[portal.MXID]
		return !exists
	})
}

func TestInvitationNoticeAndAccept(t *testing.T) {
	w := newWorld(t)
	w.portal(w.room.ID)
	other := w.srv.AddChannel("secret", "", w.bob.ID)
	login := w.br.GetCachedUserLoginByID(chattoid.MakeUserLoginID(testServer, w.alice.ID))
	mgmt, err := login.User.GetManagementRoom(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	w.srv.PublishToUser(w.alice.ID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_RoomInvitationCreated{
		RoomInvitationCreated: &realtimev1.RoomInvitationCreatedEvent{RoomId: other.ID, InvitationId: other.ID},
	}})
	// The bot can see the room's name only if alice can; the notice still
	// carries the invitation ID.
	w.waitMessage(mgmt, "accept-invite "+other.ID)
	w.sendMatrix(mgmt, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "accept-invite " + other.ID})
	w.portal(other.ID)
}

func TestSessionTerminatedReportsBadCredentials(t *testing.T) {
	w := newWorld(t)
	w.portal(w.room.ID)
	login := w.br.GetCachedUserLoginByID(chattoid.MakeUserLoginID(testServer, w.alice.ID))
	w.srv.RevokeSessions(w.alice.ID)
	w.srv.CloseRealtime(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_SESSION_TERMINATED, false, 0)
	w.eventually("bad credentials state", func() bool {
		return w.conn.loginState(login.ID) == status.StateBadCredentials
	})
}

func TestRevokedRefreshTokenReportsBadCredentials(t *testing.T) {
	w := newWorld(t)
	w.portal(w.room.ID)
	login := w.br.GetCachedUserLoginByID(chattoid.MakeUserLoginID(testServer, w.alice.ID))
	w.srv.RevokeSessions(w.alice.ID)
	w.srv.CloseRealtime(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_AUTHENTICATION_REQUIRED, true, 0)
	w.eventually("bad credentials state", func() bool {
		return w.conn.loginState(login.ID) == status.StateBadCredentials
	})
}

func TestUnsupportedReactionIsRejected(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "react weirdly")
	orig := w.waitMessage(portal.MXID, "react weirdly")
	w.eventually("db entry", func() bool { return len(w.dbMessage(msg.ID)) == 1 })
	w.sendMatrix(portal.MXID, aliceMXID, event.EventReaction, &event.ReactionEventContent{
		RelatesTo: event.RelatesTo{Type: event.RelAnnotation, EventID: orig.ID, Key: "not an emoji"},
	})
	time.Sleep(300 * time.Millisecond)
	if w.srv.CallCount("AddReaction") != 0 {
		t.Fatal("unsupported reaction reached Chatto")
	}
}

func TestOverlongMessageIsRejected(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: strings.Repeat("a", maxBodyLength+1)})
	time.Sleep(300 * time.Millisecond)
	if w.srv.CallCount("CreateMessage") != 0 {
		t.Fatal("overlong message reached Chatto")
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	w := newWorld(t)
	w.portal(w.room.ID)
	login := w.br.GetCachedUserLoginByID(chattoid.MakeUserLoginID(testServer, w.alice.ID))
	sess := *login.Metadata.(*chattoid.UserLoginMetadata).Session
	login.Logout(w.ctx)
	if w.srv.CallCount("GetViewer") == 0 {
		t.Fatal("login never talked to the server")
	}
	// The refresh token from before logout must be dead.
	_, refreshErr := w.conn.newClientMust(t).RefreshSession(w.ctx, sess)
	if refreshErr == nil {
		t.Fatal("session still valid after logout")
	}
}

func TestEmoteAndNoticeFromMatrix(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgEmote, Body: "waves"})
	w.eventually("emote on chatto", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.Body == "*waves*" {
				return true
			}
		}
		return false
	})
}

func TestChattoMentionOfUnbridgedUser(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.PostMessage(w.alice.ID, w.room.ID, "hey @bob and @all")
	evt := w.waitMessage(portal.MXID, "hey")
	content := evt.Message()
	bobGhost := w.mx.FormatGhostMXID(chattoid.MakeUserID(testServer, w.bob.ID))
	if content.Mentions == nil || !content.Mentions.Room || len(content.Mentions.UserIDs) != 1 || content.Mentions.UserIDs[0] != bobGhost {
		t.Fatalf("mentions %+v", content.Mentions)
	}
	if !strings.Contains(content.FormattedBody, string(bobGhost)) {
		t.Fatalf("no pill for bob: %q", content.FormattedBody)
	}
}

// newClientMust returns an unauthenticated client for the harness server.
func (c *Connector) newClientMust(t *testing.T) *chattoclient.Client {
	t.Helper()
	client, err := c.newClient(c.server(testServer), nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestPinsReachMatrix(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "pin me")
	evt := w.waitMessage(portal.MXID, "pin me")
	w.eventually("db entry", func() bool { return len(w.dbMessage(msg.ID)) == 1 })
	pinnedState := func() []id.EventID {
		var last []id.EventID
		for _, e := range w.mx.Events(func(e *sentEvent) bool { return e.Type == event.StatePinnedEvents && e.RoomID == portal.MXID }) {
			_ = e.Content.ParseRaw(e.Type)
			if c, ok := e.Content.Parsed.(*event.PinnedEventsEventContent); ok {
				last = c.Pinned
			}
		}
		return last
	}
	w.srv.Pin(msg.ID, true)
	w.eventually("pinned", func() bool { p := pinnedState(); return len(p) == 1 && p[0] == evt.ID })
	w.srv.Pin(msg.ID, false)
	w.eventually("unpinned", func() bool {
		return len(pinnedState()) == 0 && len(w.mx.Events(func(e *sentEvent) bool { return e.Type == event.StatePinnedEvents })) == 2
	})
}

func TestLiveOnlyRecoverySyncsRooms(t *testing.T) {
	h := newHarness(t)
	alice := h.srv.AddUser("alice", "Alice", "pw")
	room := h.srv.AddChannel("listed", "", alice.ID)
	h.srv.LiveOnly.Store(true)
	h.login(aliceMXID, "alice", "pw")
	// No snapshot arrives, so the bridge must list rooms itself.
	h.portal(room.ID)
}
