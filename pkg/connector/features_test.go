package connector

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
	realtimev1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/realtime/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattotest"
)

func TestChattoAttachmentReachesMatrix(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	png := []byte("\x89PNG\r\n\x1a\nfake image bytes")
	w.srv.PostMessage(w.bob.ID, w.room.ID, "look at this", w.srv.WithAttachment("cat.png", "image/png", png, false))
	w.waitMessage(portal.MXID, "look at this")
	var media *sentEvent
	w.eventually("image event", func() bool {
		for _, e := range w.messagesIn(portal.MXID) {
			if m := e.Message(); m != nil && m.MsgType == event.MsgImage {
				media = e
				return true
			}
		}
		return false
	})
	content := media.Message()
	if content.FileName != "cat.png" || content.Info.MimeType != "image/png" || content.Info.Size != len(png) {
		t.Fatalf("image content %+v / %+v", content, content.Info)
	}
	data, err := w.mx.BotIntent().DownloadMedia(w.ctx, content.URL, nil)
	if err != nil || !bytes.Equal(data, png) {
		t.Fatalf("uploaded media mismatch: %v", err)
	}
}

func TestProcessingVideoIsEditedWhenReady(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	video := []byte("fake mp4 bytes")
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "", w.srv.WithAttachment("clip.mp4", "video/mp4", video, true))
	placeholder := w.waitMessage(portal.MXID, "still processing")
	w.srv.FinishProcessing(msg.ID, msg.Attachments[0].ID)
	w.eventually("video edit", func() bool {
		for _, e := range w.messagesIn(portal.MXID) {
			m := e.Message()
			if m != nil && m.RelatesTo != nil && m.RelatesTo.Type == event.RelReplace && m.RelatesTo.EventID == placeholder.ID &&
				m.NewContent != nil && m.NewContent.MsgType == event.MsgVideo {
				return true
			}
		}
		return false
	})
}

func TestMatrixMediaReachesChatto(t *testing.T) {
	w := newWorld(t)
	w.srv.MaxChunkSize = 7 // force several chunks
	portal := w.portal(w.room.ID)
	data := []byte("some file contents that span several chunks")
	uri, _, err := w.mx.BotIntent().UploadMedia(w.ctx, "", data, "notes.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType:  event.MsgFile,
		Body:     "here are my notes",
		FileName: "notes.txt",
		URL:      uri,
		Info:     &event.FileInfo{MimeType: "text/plain", Size: len(data)},
	})
	var sent *chattotest.Message
	w.eventually("file on chatto", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.ActorID == w.alice.ID && len(m.Attachments) == 1 {
				sent = m
				return true
			}
		}
		return false
	})
	if sent.Body != "here are my notes" {
		t.Errorf("caption %q", sent.Body)
	}
	att := sent.Attachments[0]
	if att.Filename != "notes.txt" || !bytes.Equal(att.Data, data) {
		t.Fatalf("attachment %q %q", att.Filename, att.Data)
	}
}

func TestMatrixMediaTooLarge(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.MaxUploadSize = 4
	// Portal creation already cached the server's default limit.
	w.conn.uploadLimit.mu.Lock()
	w.conn.uploadLimit.size = 0
	w.conn.uploadLimit.mu.Unlock()
	uri, _, _ := w.mx.BotIntent().UploadMedia(w.ctx, "", []byte("too large"), "big.bin", "application/octet-stream")
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgFile, Body: "big.bin", FileName: "big.bin", URL: uri,
	})
	time.Sleep(300 * time.Millisecond)
	if w.srv.CallCount("CreateUpload") != 0 {
		t.Fatal("oversized file was uploaded")
	}
}

func TestTypingBothWays(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.PublishTyping(w.bob.ID, w.room.ID)
	bobGhost := w.mx.FormatGhostMXID(chattoid.MakeUserID(w.bob.ID))
	w.eventually("typing on matrix", func() bool {
		w.mx.mu.Lock()
		defer w.mx.mu.Unlock()
		for _, entry := range w.mx.typing {
			if strings.HasPrefix(entry, string(bobGhost)+"/"+string(portal.MXID)+"/true") {
				return true
			}
		}
		return false
	})

	w.sendMatrix(portal.MXID, aliceMXID, event.EphemeralEventTyping, &event.TypingEventContent{UserIDs: []id.UserID{aliceMXID}})
	w.eventually("typing on chatto", func() bool { return w.srv.CallCount("RefreshTypingIndicator") >= 1 })
	w.sendMatrix(portal.MXID, aliceMXID, event.EphemeralEventTyping, &event.TypingEventContent{UserIDs: nil})
	n := w.srv.CallCount("RefreshTypingIndicator")
	time.Sleep(2500 * time.Millisecond)
	if after := w.srv.CallCount("RefreshTypingIndicator"); after > n+1 {
		t.Fatalf("typing kept refreshing after stop: %d → %d", n, after)
	}
}

func TestReadReceiptReachesChatto(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	msg := w.srv.PostMessage(w.bob.ID, w.room.ID, "read me")
	evt := w.waitMessage(portal.MXID, "read me")
	w.eventually("db entry", func() bool { return len(w.dbMessage(msg.ID)) == 1 })
	w.sendMatrix(portal.MXID, aliceMXID, event.EphemeralEventReceipt, &event.ReceiptEventContent{
		evt.ID: {event.ReceiptTypeRead: {aliceMXID: {Timestamp: time.Now()}}},
	})
	w.eventually("read marker", func() bool { return w.srv.ReadMarker(w.alice.ID, w.room.ID) == msg.ID })
}

func TestRoomRenameBothWays(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.UpdateRoom(w.room.ID, "renamed", "a new topic")
	w.eventually("name state", func() bool {
		return len(w.mx.Events(func(e *sentEvent) bool {
			if e.Type != event.StateRoomName || e.RoomID != portal.MXID {
				return false
			}
			_ = e.Content.ParseRaw(e.Type)
			c, _ := e.Content.Parsed.(*event.RoomNameEventContent)
			return c != nil && c.Name == "renamed"
		})) > 0
	})
	w.sendMatrix(portal.MXID, aliceMXID, event.StateRoomName, &event.RoomNameEventContent{Name: "from matrix"})
	w.eventually("name on chatto", func() bool { return w.srv.Room(w.room.ID).Name == "from matrix" })
	w.sendMatrix(portal.MXID, aliceMXID, event.StateTopic, &event.TopicEventContent{Topic: "matrix topic"})
	w.eventually("topic on chatto", func() bool { return w.srv.Room(w.room.ID).Description == "matrix topic" })
}

func TestDMPortal(t *testing.T) {
	h := newHarness(t)
	alice := h.srv.AddUser("alice", "Alice", "pw")
	bob := h.srv.AddUser("bob", "Bob", "pw")
	dm := h.srv.AddDM(alice.ID, bob.ID)
	h.login(aliceMXID, "alice", "pw")
	portal := h.portal(dm.ID)
	h.eventually("DM portal metadata", func() bool {
		p, _ := h.br.DB.Portal.GetByKey(h.ctx, chattoid.MakePortalKey(dm.ID))
		return p.RoomType == database.RoomTypeDM && p.OtherUserID == chattoid.MakeUserID(bob.ID)
	})
	h.srv.PostMessage(bob.ID, dm.ID, "psst")
	h.waitMessage(portal.MXID, "psst")
}

func TestGroupDMPortal(t *testing.T) {
	h := newHarness(t)
	alice := h.srv.AddUser("alice", "Alice", "pw")
	bob := h.srv.AddUser("bob", "Bob", "pw")
	carol := h.srv.AddUser("carol", "Carol", "pw")
	dm := h.srv.AddDM(alice.ID, bob.ID, carol.ID)
	h.login(aliceMXID, "alice", "pw")
	h.portal(dm.ID)
	h.eventually("group DM portal type", func() bool {
		p, _ := h.br.DB.Portal.GetByKey(h.ctx, chattoid.MakePortalKey(dm.ID))
		return p.RoomType == database.RoomTypeGroupDM
	})
}

func TestRoomGroupBecomesSpace(t *testing.T) {
	h := newHarness(t)
	alice := h.srv.AddUser("alice", "Alice", "pw")
	group := h.srv.AddGroup("Engineering")
	room := h.srv.AddChannel("backend", group.ID, alice.ID)
	h.login(aliceMXID, "alice", "pw")
	portal := h.portal(room.ID)
	space := h.portalByID(chattoidGroupKey(group.ID))
	h.eventually("space portal metadata", func() bool {
		p, _ := h.br.DB.Portal.GetByKey(h.ctx, chattoidGroupKey(group.ID))
		return p.RoomType == database.RoomTypeSpace && p.Name == "Engineering"
	})
	h.eventually("space child", func() bool {
		return len(h.mx.Events(func(e *sentEvent) bool {
			return e.Type == event.StateSpaceChild && e.RoomID == space.MXID && e.StateKey != nil && *e.StateKey == string(portal.MXID)
		})) > 0
	})
}

func TestStartDMFromMatrix(t *testing.T) {
	w := newWorld(t)
	login := w.br.GetCachedUserLoginByID(chattoid.MakeUserLoginID(w.alice.ID))
	resp, err := login.Client.(*ChattoClient).ResolveIdentifier(w.ctx, "@bob", true)
	if err != nil {
		t.Fatal(err)
	}
	if resp.UserID != chattoid.MakeUserID(w.bob.ID) || resp.Chat == nil || resp.Chat.PortalKey.ID == "" {
		t.Fatalf("resolve response %+v", resp)
	}
	unknown, err := login.Client.(*ChattoClient).ResolveIdentifier(w.ctx, "nobody", false)
	if err != nil || unknown != nil {
		t.Fatalf("unknown user: %v %v", unknown, err)
	}
}

func TestMembershipChanges(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	carol := w.srv.AddUser("carol", "Carol", "pw")
	w.srv.AddMember(w.room.ID, carol.ID)
	carolGhost := w.mx.FormatGhostMXID(chattoid.MakeUserID(carol.ID))
	w.eventually("carol joined", func() bool {
		w.mx.mu.Lock()
		defer w.mx.mu.Unlock()
		return w.mx.rooms[portal.MXID].members[carolGhost] == event.MembershipJoin
	})
}

func TestCallNotices(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.Publish(w.room.ID, w.bob.ID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_VoiceCallStarted{
		VoiceCallStarted: &realtimev1.VoiceCallStartedEvent{RoomId: w.room.ID, CallId: "C1"},
	}})
	evt := w.waitMessage(portal.MXID, "Started a call")
	if evt.Message().MsgType != event.MsgNotice {
		t.Fatalf("call notice msgtype %q", evt.Message().MsgType)
	}
}

func TestPresenceSync(t *testing.T) {
	w := newWorld(t)
	w.portal(w.room.ID)
	w.srv.Publish("", w.bob.ID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_PresenceChanged{
		PresenceChanged: &realtimev1.PresenceChangedEvent{Status: apiv1.PresenceStatus_PRESENCE_STATUS_AWAY},
	}})
	bobGhost := w.mx.FormatGhostMXID(chattoid.MakeUserID(w.bob.ID))
	w.eventually("presence", func() bool {
		w.mx.mu.Lock()
		defer w.mx.mu.Unlock()
		return w.mx.presences[bobGhost].Presence == event.PresenceUnavailable
	})
}

func TestBackfillOnFirstSync(t *testing.T) {
	h := newHarness(t, withBackfill(50))
	alice := h.srv.AddUser("alice", "Alice", "pw")
	bob := h.srv.AddUser("bob", "Bob", "pw")
	room := h.srv.AddChannel("history", "", alice.ID, bob.ID)
	for _, body := range []string{"old one", "old two", "old three"} {
		h.srv.PostMessage(bob.ID, room.ID, body)
	}
	root := h.srv.PostMessage(bob.ID, room.ID, "old root")
	h.srv.PostMessage(alice.ID, room.ID, "old thread reply", chattotest.InThread(root.ID))
	h.login(aliceMXID, "alice", "pw")
	portal := h.portal(room.ID)
	for _, body := range []string{"old one", "old two", "old three", "old root"} {
		h.waitMessage(portal.MXID, body)
	}
	msgs := h.messagesIn(portal.MXID)
	order := []string{}
	for _, e := range msgs {
		if m := e.Message(); m != nil && strings.HasPrefix(m.Body, "old ") && !strings.Contains(m.Body, "thread") {
			order = append(order, m.Body)
		}
	}
	if strings.Join(order, ",") != "old one,old two,old three,old root" {
		t.Fatalf("backfill order %v", order)
	}
}

func TestForwardFillAfterOutage(t *testing.T) {
	w := newWorld(t, withBackfill(50))
	portal := w.portal(w.room.ID)
	w.srv.PostMessage(w.bob.ID, w.room.ID, "before outage")
	w.waitMessage(portal.MXID, "before outage")
	// Cursors expire and the socket drops: events in between are not
	// replayed, so the bridge must forward-fill from the timeline.
	w.srv.ExpireCursors()
	w.srv.DropRealtime()
	w.srv.PostMessage(w.bob.ID, w.room.ID, "during outage")
	w.waitMessage(portal.MXID, "during outage")
}

func TestTokenLoginWithBotKey(t *testing.T) {
	h := newHarness(t)
	owner := h.srv.AddUser("owner", "Owner", "pw")
	bot, key := h.srv.AddBot("bridgebot", "Bridge Bot", owner.ID)
	room := h.srv.AddChannel("bots", "", owner.ID, bot.ID)
	user, _ := h.br.GetUserByMXID(h.ctx, "@botowner:test")
	proc, err := h.conn.CreateLogin(h.ctx, user, LoginFlowToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Start(h.ctx); err != nil {
		t.Fatal(err)
	}
	step, err := proc.(bridgev2.LoginProcessUserInput).SubmitUserInput(h.ctx, map[string]string{"token": key})
	if err != nil {
		t.Fatal(err)
	}
	ul := step.CompleteParams.UserLogin
	meta := ul.Metadata.(*chattoid.UserLoginMetadata)
	if !meta.IsBot || meta.Kind != chattoid.CredentialBotKey || ul.ID != chattoid.MakeUserLoginID(bot.ID) {
		t.Fatalf("bot login metadata %+v", meta)
	}
	portal := h.portal(room.ID)
	h.srv.PostMessage(owner.ID, room.ID, "hello bot")
	h.waitMessage(portal.MXID, "hello bot")
}

func TestLoginErrors(t *testing.T) {
	h := newHarness(t)
	h.srv.AddUser("alice", "Alice", "pw")
	user, _ := h.br.GetUserByMXID(h.ctx, aliceMXID)
	proc, _ := h.conn.CreateLogin(h.ctx, user, LoginFlowPassword)
	_, _ = proc.Start(h.ctx)
	if _, err := proc.(bridgev2.LoginProcessUserInput).SubmitUserInput(h.ctx, map[string]string{"login": "alice", "password": "nope"}); !isRespError(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	tok, _ := h.conn.CreateLogin(h.ctx, user, LoginFlowToken)
	_, _ = tok.Start(h.ctx)
	if _, err := tok.(bridgev2.LoginProcessUserInput).SubmitUserInput(h.ctx, map[string]string{"token": "cht_BK_bogus"}); !isRespError(err, ErrInvalidToken) {
		t.Fatalf("bogus bot key: %v", err)
	}
	if _, err := h.conn.CreateLogin(h.ctx, user, "nope"); err == nil || err.Error() != bridgev2.ErrInvalidLoginFlowID.Error() {
		t.Fatalf("unknown flow: %v", err)
	}
}

func TestRefreshTokenLoginTakesOverSession(t *testing.T) {
	h := newHarness(t)
	alice := h.srv.AddUser("alice", "Alice", "pw")
	room := h.srv.AddChannel("general", "", alice.ID)
	_, refresh := h.srv.IssueSession(alice.ID)
	user, _ := h.br.GetUserByMXID(h.ctx, aliceMXID)
	proc, _ := h.conn.CreateLogin(h.ctx, user, LoginFlowToken)
	_, _ = proc.Start(h.ctx)
	step, err := proc.(bridgev2.LoginProcessUserInput).SubmitUserInput(h.ctx, map[string]string{"token": refresh})
	if err != nil {
		t.Fatal(err)
	}
	meta := step.CompleteParams.UserLogin.Metadata.(*chattoid.UserLoginMetadata)
	if meta.Kind != chattoid.CredentialSession || meta.Session == nil || meta.Session.RefreshToken == refresh {
		t.Fatalf("session not rotated: %+v", meta.Session)
	}
	h.portal(room.ID)
}

func TestAccessTokenExpiryIsTransparent(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	w.srv.ExpireAccessTokens()
	w.srv.CloseRealtime(realtimev1.RealtimeCloseCode_REALTIME_CLOSE_CODE_AUTHENTICATION_REQUIRED, true, 0)
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "after expiry"})
	w.eventually("message after token refresh", func() bool {
		for _, m := range w.srv.Messages(w.room.ID) {
			if m.Body == "after expiry" {
				return true
			}
		}
		return false
	})
	w.srv.PostMessage(w.bob.ID, w.room.ID, "still receiving")
	w.waitMessage(portal.MXID, "still receiving")
}

func TestGrantBotPermissionsCommand(t *testing.T) {
	w := newWorld(t)
	portal := w.portal(w.room.ID)
	bot, _ := w.srv.AddBot("helper", "Helper", w.alice.ID)
	w.sendMatrix(portal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "!chatto grant-bot-permissions helper"})
	w.eventually("permissions granted", func() bool {
		n := 0
		for _, p := range w.srv.PermissionsFor(bot.ID) {
			if p.ScopeID == w.room.ID {
				n++
			}
		}
		return n == len(BotPermissions)
	})
	w.eventually("bot added to room", func() bool {
		r := w.srv.Room(w.room.ID)
		for _, m := range r.Members {
			if m == bot.ID {
				return true
			}
		}
		return false
	})
}

func chattoidGroupKey(groupID string) networkid.PortalKey {
	return networkid.PortalKey{ID: chattoid.MakeGroupPortalID(groupID)}
}

func isRespError(err error, want bridgev2.RespError) bool {
	var got bridgev2.RespError
	return errors.As(err, &got) && got.ErrCode == want.ErrCode
}
