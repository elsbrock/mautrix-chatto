package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/elsbrock/mautrix-chatto/pkg/bridgemetrics"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
	realtimev1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/realtime/v1"
)

// typingTimeout matches Chatto's receiver-side typing expiry.
const typingTimeout = 6 * time.Second

// HandleState implements chattoclient.RealtimeHandler.
func (c *ChattoClient) HandleState(state chattoclient.RealtimeState, err error) {
	switch state {
	case chattoclient.RealtimeConnected:
		c.sendState(status.BridgeState{StateEvent: status.StateConnected})
	case chattoclient.RealtimeTransientDisconnect:
		msg := "Lost connection to Chatto"
		if err != nil {
			msg = fmt.Sprintf("Lost connection to Chatto: %v", err)
		}
		c.sendState(status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      "chatto-realtime-disconnected",
			Message:    msg,
		})
	case chattoclient.RealtimeBadCredentials:
		// Reported by the Run loop owner, see Connect.
	}
}

// SaveCursor implements chattoclient.RealtimeHandler. Cursors are written at
// most every 30 seconds; losing one only costs a snapshot on restart.
func (c *ChattoClient) SaveCursor(cursor string) {
	c.metaLock.Lock()
	defer c.metaLock.Unlock()
	c.meta().RealtimeCursor = cursor
	if time.Since(c.lastCursorSave) < 30*time.Second {
		return
	}
	c.lastCursorSave = time.Now()
	ctx := c.UserLogin.Log.WithContext(context.Background())
	if err := c.UserLogin.Save(ctx); err != nil {
		c.UserLogin.Log.Warn().Err(err).Msg("Failed to save realtime cursor")
	}
}

// HandleSnapshot implements chattoclient.RealtimeHandler. A snapshot means
// the stream could not resume, so every visible room is resynced and
// forward-filled.
func (c *ChattoClient) HandleSnapshot(ctx context.Context, snap *realtimev1.RealtimeSnapshot) {
	c.users.Put(usersFromDirectory(snap.GetUsers())...)
	for _, rws := range snap.GetRooms() {
		if !rws.GetViewerState().GetIsMember() {
			continue
		}
		c.queueChatResync(ctx, rws, true)
	}
}

// HandleCaughtUp implements chattoclient.RealtimeHandler.
func (c *ChattoClient) HandleCaughtUp(ctx context.Context, recovery realtimev1.RealtimeRecovery) {
	if recovery == realtimev1.RealtimeRecovery_REALTIME_RECOVERY_LIVE_ONLY {
		// Neither a replay nor a snapshot: list the rooms ourselves. This
		// runs on the realtime goroutine like all other remote events.
		c.syncAllRooms(ctx)
	}
}

func usersFromDirectory(members []*apiv1.DirectoryMember) []*apiv1.User {
	users := make([]*apiv1.User, 0, len(members))
	for _, m := range members {
		users = append(users, m.GetUser())
	}
	return users
}

// syncAllRooms resyncs every room the login is a member of.
func (c *ChattoClient) syncAllRooms(ctx context.Context) {
	offset := int32(0)
	for {
		resp, err := c.api.RoomDirectory.ListRooms(ctx, connect.NewRequest(&apiv1.ListRoomsRequest{
			Scope:         apiv1.RoomDirectoryScope_ROOM_DIRECTORY_SCOPE_ALL,
			ArchiveFilter: apiv1.RoomArchiveFilter_ROOM_ARCHIVE_FILTER_ACTIVE,
			Page:          &apiv1.PageRequest{Limit: 100, Offset: offset},
		}))
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to list rooms")
			return
		}
		for _, rws := range resp.Msg.GetRooms() {
			if rws.GetViewerState().GetIsMember() {
				c.queueChatResync(ctx, rws, true)
			}
		}
		if !resp.Msg.GetPage().GetHasMore() || len(resp.Msg.GetRooms()) == 0 {
			return
		}
		offset += int32(len(resp.Msg.GetRooms()))
	}
}

// queueChatResync syncs a room's info and, when backfill is set, lets
// bridgev2 forward-fill messages missed while disconnected.
func (c *ChattoClient) queueChatResync(ctx context.Context, rws *apiv1.RoomWithViewerState, backfill bool) {
	room := rws.GetRoom()
	evt := &simplevent.ChatResync{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventChatResync,
			PortalKey:    c.portalKey(room.GetId()),
			CreatePortal: true,
			LogContext: func(lc zerolog.Context) zerolog.Context {
				return lc.Str("room_id", room.GetId())
			},
		},
		GetChatInfoFunc: func(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
			return c.roomToChatInfo(ctx, rws, true)
		},
	}
	if backfill {
		evt.CheckNeedsBackfillFunc = func(ctx context.Context, latest *database.Message) (bool, error) {
			return c.roomNeedsBackfill(ctx, room.GetId(), latest)
		}
	}
	c.UserLogin.QueueRemoteEvent(evt)
}

// roomNeedsBackfill compares the newest message on Chatto with the newest
// bridged one.
func (c *ChattoClient) roomNeedsBackfill(ctx context.Context, roomID string, latest *database.Message) (bool, error) {
	resp, err := c.api.Rooms.GetRoomEvents(ctx, connect.NewRequest(&apiv1.GetRoomEventsRequest{RoomId: roomID, Limit: 20}))
	if err != nil {
		return false, err
	}
	// Compare times rather than IDs: the newest bridged message may be a
	// thread reply or a notice, which the room timeline does not list.
	for _, evt := range resp.Msg.GetPage().GetEvents() {
		msg := evt.GetMessagePosted().GetMessage()
		if msg == nil || msg.GetEchoOfEventId() != "" {
			continue
		}
		if latest == nil {
			return true, nil
		}
		if evt.GetCreatedAt().AsTime().After(latest.Timestamp) {
			return true, nil
		}
		if last := msg.GetThread().GetLastReplyAt(); last != nil && last.AsTime().After(latest.Timestamp) {
			return true, nil
		}
	}
	return false, nil
}

// isTransient reports whether an RPC error is worth retrying by replaying
// the realtime event.
func isTransient(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return true // network errors
	}
	switch cerr.Code() {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeResourceExhausted,
		connect.CodeAborted, connect.CodeInternal, connect.CodeUnknown:
		return true
	}
	return false
}

// HandleEvent implements chattoclient.RealtimeHandler. It only returns an
// error for transient failures, which makes the client replay the event.
func (c *ChattoClient) HandleEvent(ctx context.Context, evt *realtimev1.RealtimeEvent) error {
	log := zerolog.Ctx(ctx).With().
		Str("event_id", evt.GetId()).
		Str("event_type", chattoclient.EventType(evt)).
		Logger()
	ctx = log.WithContext(ctx)
	actor := evt.GetActorId()
	ts := evt.GetCreatedAt().AsTime()
	meta := func(t bridgev2.RemoteEventType, roomID string) simplevent.EventMeta {
		return simplevent.EventMeta{
			Type:      t,
			PortalKey: c.portalKey(roomID),
			Sender:    c.makeEventSender(actor),
			Timestamp: ts,
			LogContext: func(lc zerolog.Context) zerolog.Context {
				return lc.Str("chatto_event_id", evt.GetId()).Str("room_id", roomID)
			},
		}
	}
	switch e := evt.GetEvent().(type) {
	case *realtimev1.RealtimeEvent_MessagePosted:
		return c.handleMessagePosted(ctx, evt, e.MessagePosted)
	case *realtimev1.RealtimeEvent_MessageEdited:
		return c.handleMessageChanged(ctx, evt, e.MessageEdited.GetRoomId(), e.MessageEdited.GetMessageEventId())
	case *realtimev1.RealtimeEvent_AssetProcessingSucceeded:
		return c.handleMessageChanged(ctx, evt, e.AssetProcessingSucceeded.GetRoomId(), e.AssetProcessingSucceeded.GetMessageEventId())
	case *realtimev1.RealtimeEvent_AssetProcessingFailed:
		return c.handleMessageChanged(ctx, evt, e.AssetProcessingFailed.GetRoomId(), e.AssetProcessingFailed.GetMessageEventId())
	case *realtimev1.RealtimeEvent_AssetDeleted:
		return c.handleMessageChanged(ctx, evt, e.AssetDeleted.GetRoomId(), e.AssetDeleted.GetMessageEventId())
	case *realtimev1.RealtimeEvent_MessageRetracted:
		c.UserLogin.QueueRemoteEvent(&simplevent.MessageRemove{
			EventMeta:     meta(bridgev2.RemoteEventMessageRemove, e.MessageRetracted.GetRoomId()),
			TargetMessage: c.messageID(e.MessageRetracted.GetMessageEventId()),
		})
		bridgemetrics.MessagesBridged.WithLabelValues("to_matrix", "redaction").Inc()
	case *realtimev1.RealtimeEvent_ReactionAdded:
		c.UserLogin.QueueRemoteEvent(&simplevent.Reaction{
			EventMeta:     meta(bridgev2.RemoteEventReaction, e.ReactionAdded.GetRoomId()),
			TargetMessage: c.messageID(e.ReactionAdded.GetMessageEventId()),
			EmojiID:       chattoid.MakeEmojiID(e.ReactionAdded.GetEmoji()),
			Emoji:         emojiFromShortcode(e.ReactionAdded.GetEmoji()),
		})
		bridgemetrics.MessagesBridged.WithLabelValues("to_matrix", "reaction").Inc()
	case *realtimev1.RealtimeEvent_ReactionRemoved:
		c.UserLogin.QueueRemoteEvent(&simplevent.Reaction{
			EventMeta:     meta(bridgev2.RemoteEventReactionRemove, e.ReactionRemoved.GetRoomId()),
			TargetMessage: c.messageID(e.ReactionRemoved.GetMessageEventId()),
			EmojiID:       chattoid.MakeEmojiID(e.ReactionRemoved.GetEmoji()),
		})
		bridgemetrics.MessagesBridged.WithLabelValues("to_matrix", "reaction_remove").Inc()
	case *realtimev1.RealtimeEvent_UserTyping:
		if actor == c.userID {
			return nil
		}
		c.UserLogin.QueueRemoteEvent(&simplevent.Typing{
			EventMeta: meta(bridgev2.RemoteEventTyping, e.UserTyping.GetRoomId()),
			Timeout:   typingTimeout,
			Type:      bridgev2.TypingTypeText,
		})
	case *realtimev1.RealtimeEvent_RoomReadStateChanged:
		c.handleOwnReadState(ctx, e.RoomReadStateChanged.GetRoomId(), ts)
	case *realtimev1.RealtimeEvent_PresenceChanged:
		c.handlePresence(ctx, actor, e.PresenceChanged.GetStatus())
	case *realtimev1.RealtimeEvent_UserProfileChanged:
		c.handleProfileChanged(ctx, e.UserProfileChanged.GetUserId())
	case *realtimev1.RealtimeEvent_RoomCreated:
		c.resyncRoom(ctx, e.RoomCreated.GetRoomId(), false)
	case *realtimev1.RealtimeEvent_RoomUpdated:
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta: meta(bridgev2.RemoteEventChatInfoChange, e.RoomUpdated.GetRoomId()),
			ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: &bridgev2.ChatInfo{
				Name:  ptr.Ptr(e.RoomUpdated.GetName()),
				Topic: ptr.Ptr(e.RoomUpdated.GetDescription()),
			}},
		})
	case *realtimev1.RealtimeEvent_RoomArchived:
		c.resyncRoom(ctx, e.RoomArchived.GetRoomId(), false)
		c.sendRoomNotice(ctx, e.RoomArchived.GetRoomId(), actor, ts, "This room was archived on Chatto. New messages cannot be sent until it is unarchived.")
	case *realtimev1.RealtimeEvent_RoomUnarchived:
		c.resyncRoom(ctx, e.RoomUnarchived.GetRoomId(), false)
		c.sendRoomNotice(ctx, e.RoomUnarchived.GetRoomId(), actor, ts, "This room was unarchived on Chatto.")
	case *realtimev1.RealtimeEvent_RoomDeleted:
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatDelete{
			EventMeta: meta(bridgev2.RemoteEventChatDelete, e.RoomDeleted.GetRoomId()),
		})
	case *realtimev1.RealtimeEvent_RoomThreadingModeChanged, *realtimev1.RealtimeEvent_RoomUniversalChanged:
		roomID := ""
		if tm := evt.GetRoomThreadingModeChanged(); tm != nil {
			roomID = tm.GetRoomId()
		} else {
			roomID = evt.GetRoomUniversalChanged().GetRoomId()
		}
		c.resyncRoom(ctx, roomID, false)
	case *realtimev1.RealtimeEvent_UserJoinedRoom:
		roomID := e.UserJoinedRoom.GetRoomId()
		if actor == c.userID || actor == "" {
			c.resyncRoom(ctx, roomID, true)
			return nil
		}
		if c.Main.IsGhostBot(ctx, c.ghostID(actor)) {
			return nil
		}
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta: meta(bridgev2.RemoteEventChatInfoChange, roomID),
			ChatInfoChange: &bridgev2.ChatInfoChange{MemberChanges: &bridgev2.ChatMemberList{
				MemberMap: bridgev2.ChatMemberMap{}.Set(c.makeMember(actor)),
			}},
		})
	case *realtimev1.RealtimeEvent_UserLeftRoom:
		roomID := e.UserLeftRoom.GetRoomId()
		if actor == c.userID {
			c.UserLogin.QueueRemoteEvent(&simplevent.ChatDelete{
				EventMeta: meta(bridgev2.RemoteEventChatDelete, roomID),
				OnlyForMe: true,
			})
			return nil
		}
		if c.Main.IsGhostBot(ctx, c.ghostID(actor)) {
			return nil
		}
		member := c.makeMember(actor)
		member.Membership = event.MembershipLeave
		c.UserLogin.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta: meta(bridgev2.RemoteEventChatInfoChange, roomID),
			ChatInfoChange: &bridgev2.ChatInfoChange{MemberChanges: &bridgev2.ChatMemberList{
				MemberMap: bridgev2.ChatMemberMap{}.Set(member),
			}},
		})
	case *realtimev1.RealtimeEvent_VoiceCallStarted:
		if c.Main.Config.CallNotices {
			c.sendRoomNotice(ctx, e.VoiceCallStarted.GetRoomId(), actor, ts, "📞 Started a call on Chatto")
		}
	case *realtimev1.RealtimeEvent_VoiceCallEnded:
		if c.Main.Config.CallNotices {
			c.sendRoomNotice(ctx, e.VoiceCallEnded.GetRoomId(), "", ts, "📞 The call on Chatto ended")
		}
	case *realtimev1.RealtimeEvent_MessagePinned:
		c.syncPins(ctx, e.MessagePinned.GetRoomId())
	case *realtimev1.RealtimeEvent_MessageUnpinned:
		c.syncPins(ctx, e.MessageUnpinned.GetRoomId())
	case *realtimev1.RealtimeEvent_RoomLayoutChanged:
		c.ensureGroupSpaces(ctx)
		c.syncAllRooms(ctx)
	case *realtimev1.RealtimeEvent_RoomInvitationCreated:
		c.handleInvitation(ctx, e.RoomInvitationCreated.GetRoomId(), e.RoomInvitationCreated.GetInvitationId())
	default:
		log.Trace().Msg("Ignoring realtime event")
	}
	return nil
}

// mayBeBridgeSend reports whether a Chatto user's message could have been
// sent by this bridge: any logged-in account or ghost bot. Only those wait
// for in-flight sends, so other users' messages are never delayed.
func (c *ChattoClient) mayBeBridgeSend(ctx context.Context, actor string) bool {
	if actor == c.userID {
		return true
	}
	if c.Main.Bridge.GetCachedUserLoginByID(chattoid.MakeUserLoginID(c.serverID, actor)) != nil {
		return true
	}
	return c.Main.IsGhostBot(ctx, c.ghostID(actor))
}

func (c *ChattoClient) handleMessagePosted(ctx context.Context, evt *realtimev1.RealtimeEvent, posted *realtimev1.MessagePostedEvent) error {
	roomID := posted.GetRoomId()
	if posted.GetEchoOfEventId() != "" {
		c.handleThreadEcho(ctx, evt, posted)
		return nil
	}
	if c.mayBeBridgeSend(ctx, evt.GetActorId()) && c.Main.echoes.IsEcho(ctx, string(c.portalKey(roomID).ID), string(c.messageID(evt.GetId()))) {
		zerolog.Ctx(ctx).Debug().Msg("Ignoring echo of message sent by the bridge")
		return nil
	}
	resp, err := c.api.Messages.GetMessage(ctx, connect.NewRequest(&apiv1.GetMessageRequest{RoomId: roomID, EventId: evt.GetId()}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			// Already retracted.
			return nil
		}
		bridgemetrics.BridgeErrors.WithLabelValues("to_matrix", "message").Inc()
		zerolog.Ctx(ctx).Err(err).Msg("Failed to fetch posted message")
		if isTransient(ctx, err) {
			return err
		}
		return nil
	}
	msg := resp.Msg.GetMessage()
	c.UserLogin.QueueRemoteEvent(&simplevent.Message[*apiv1.Message]{
		EventMeta: simplevent.EventMeta{
			Type:         bridgev2.RemoteEventMessage,
			PortalKey:    c.portalKey(roomID),
			Sender:       c.makeEventSender(msg.GetActorId()),
			Timestamp:    msg.GetCreatedAt().AsTime(),
			CreatePortal: true,
			LogContext: func(lc zerolog.Context) zerolog.Context {
				return lc.Str("message_id", msg.GetId()).Str("room_id", roomID)
			},
		},
		Data:               msg,
		ID:                 c.messageID(msg.GetId()),
		ConvertMessageFunc: c.convertMessageFunc,
	})
	bridgemetrics.MessagesBridged.WithLabelValues("to_matrix", "message").Inc()
	return nil
}

func (c *ChattoClient) convertMessageFunc(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *apiv1.Message) (*bridgev2.ConvertedMessage, error) {
	return c.convertMessage(ctx, portal, intent, msg), nil
}

func (c *ChattoClient) convertEditFunc(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, msg *apiv1.Message) (*bridgev2.ConvertedEdit, error) {
	return c.convertEdit(ctx, portal, intent, existing, msg), nil
}

// handleThreadEcho handles a thread reply that was also sent to the channel.
// The reply itself arrives as its own event in the thread.
func (c *ChattoClient) handleThreadEcho(ctx context.Context, evt *realtimev1.RealtimeEvent, posted *realtimev1.MessagePostedEvent) {
	if c.Main.Config.ThreadEcho != "notice" {
		return
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessage,
			PortalKey: c.portalKey(posted.GetRoomId()),
			Sender:    c.makeEventSender(evt.GetActorId()),
			Timestamp: evt.GetCreatedAt().AsTime(),
		},
		ID:   c.messageID(evt.GetId()),
		Data: posted.GetEchoOfEventId(),
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, target string) (*bridgev2.ConvertedMessage, error) {
			return &bridgev2.ConvertedMessage{
				ReplyTo: &networkid.MessageOptionalPartID{MessageID: c.messageID(target)},
				Parts: []*bridgev2.ConvertedMessagePart{{
					Type:    event.EventMessage,
					Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: "↪ replied in a thread"},
				}},
			}, nil
		},
	})
}

// handleMessageChanged refetches a message after an edit or asset update and
// queues an edit.
func (c *ChattoClient) handleMessageChanged(ctx context.Context, evt *realtimev1.RealtimeEvent, roomID, messageID string) error {
	if roomID == "" || messageID == "" {
		return nil
	}
	resp, err := c.api.Messages.GetMessage(ctx, connect.NewRequest(&apiv1.GetMessageRequest{RoomId: roomID, EventId: messageID}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil
		}
		zerolog.Ctx(ctx).Err(err).Str("message_id", messageID).Msg("Failed to fetch changed message")
		if isTransient(ctx, err) {
			return err
		}
		return nil
	}
	msg := resp.Msg.GetMessage()
	ts := evt.GetCreatedAt().AsTime()
	if msg.GetUpdatedAt() != nil {
		ts = msg.GetUpdatedAt().AsTime()
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Message[*apiv1.Message]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventEdit,
			PortalKey: c.portalKey(roomID),
			Sender:    c.makeEventSender(msg.GetActorId()),
			Timestamp: ts,
		},
		Data:            msg,
		ID:              c.messageID(msg.GetId()),
		TargetMessage:   c.messageID(msg.GetId()),
		ConvertEditFunc: c.convertEditFunc,
	})
	bridgemetrics.MessagesBridged.WithLabelValues("to_matrix", "edit").Inc()
	return nil
}

// resyncRoom refetches a room and queues a resync.
func (c *ChattoClient) resyncRoom(ctx context.Context, roomID string, backfill bool) {
	resp, err := c.api.RoomDirectory.GetRoom(ctx, connect.NewRequest(&apiv1.GetRoomRequest{RoomId: roomID}))
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Str("room_id", roomID).Msg("Failed to fetch room for resync")
		return
	}
	rws := resp.Msg.GetRoom()
	if !rws.GetViewerState().GetIsMember() {
		return
	}
	c.queueChatResync(ctx, rws, backfill)
}

// sendRoomNotice posts a bridge-generated notice into a portal, as the given
// Chatto user or as the bridge bot when actor is empty.
func (c *ChattoClient) sendRoomNotice(ctx context.Context, roomID, actor string, ts time.Time, text string) {
	sender := c.makeEventSender(actor)
	if actor == "" {
		sender = bridgev2.EventSender{}
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Message[string]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventMessage,
			PortalKey: c.portalKey(roomID),
			Sender:    sender,
			Timestamp: ts,
		},
		ID:   chattoid.MakeNoticeMessageID(c.serverID, roomID, strconv.FormatInt(ts.UnixNano(), 10)),
		Data: text,
		ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, text string) (*bridgev2.ConvertedMessage, error) {
			return &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
				Type:    event.EventMessage,
				Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: text},
			}}}, nil
		},
	})
}

// handleOwnReadState mirrors reads done on another Chatto client onto the
// user's Matrix account (only visible with double puppeting).
func (c *ChattoClient) handleOwnReadState(ctx context.Context, roomID string, ts time.Time) {
	resp, err := c.api.Rooms.GetRoomReadState(ctx, connect.NewRequest(&apiv1.GetRoomReadStateRequest{RoomId: roomID}))
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to fetch room read state")
		return
	}
	marker := resp.Msg.GetState().GetMarker()
	if marker.GetLastReadEventId() == "" {
		return
	}
	readAt := ts
	if marker.GetLastReadAt() != nil {
		readAt = marker.GetLastReadAt().AsTime()
	}
	c.UserLogin.QueueRemoteEvent(&simplevent.Receipt{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventReadReceipt,
			PortalKey: c.portalKey(roomID),
			Sender:    c.makeEventSender(c.userID),
			Timestamp: readAt,
		},
		LastTarget: c.messageID(marker.GetLastReadEventId()),
	})
}

func (c *ChattoClient) handleProfileChanged(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	c.users.Invalidate(userID)
	user, err := c.users.Get(ctx, userID)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Str("user_id", userID).Msg("Failed to fetch changed profile")
		return
	}
	if userID == c.userID {
		c.updateRemoteProfile(ctx, user)
	}
	ghost, err := c.Main.Bridge.GetGhostByID(ctx, c.ghostID(userID))
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get ghost")
		return
	}
	ghost.UpdateInfo(ctx, c.userToUserInfo(user))
	if c.Main.Config.SyncPresence {
		c.setGhostPresence(ctx, ghost, user.GetPresenceStatus(), user.GetCustomStatus())
	}
}

func (c *ChattoClient) handlePresence(ctx context.Context, userID string, presence apiv1.PresenceStatus) {
	if !c.Main.Config.SyncPresence || userID == "" {
		return
	}
	ghost, err := c.Main.Bridge.GetGhostByID(ctx, c.ghostID(userID))
	if err != nil {
		return
	}
	var custom *apiv1.CustomUserStatus
	if user, err := c.users.Get(ctx, userID); err == nil {
		custom = user.GetCustomStatus()
	}
	c.setGhostPresence(ctx, ghost, presence, custom)
}

// handleInvitation tells the user about a Chatto room invitation, which
// they can accept with the accept-invite command.
func (c *ChattoClient) handleInvitation(ctx context.Context, roomID, invitationID string) {
	if c.isBot() {
		return
	}
	resp, err := c.api.RoomDirectory.GetRoom(ctx, connect.NewRequest(&apiv1.GetRoomRequest{RoomId: roomID}))
	name := roomID
	if err == nil {
		name = resp.Msg.GetRoom().GetRoom().GetName()
	}
	cmd := "accept-invite " + invitationID
	where := "Chatto"
	if len(c.Main.Config.Servers) > 1 {
		cmd += " --server " + c.serverID
		where = c.serverID
	}
	c.notifyUser(ctx, fmt.Sprintf("You were invited to #%s on %s. Run `%s` to join.", name, where, cmd))
}

// notifyUser sends a notice to the user's management room.
func (c *ChattoClient) notifyUser(ctx context.Context, text string) {
	user := c.UserLogin.User
	roomID, err := user.GetManagementRoom(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get management room")
		return
	}
	_, err = c.Main.Bridge.Bot.SendMessage(ctx, roomID, event.EventMessage, &event.Content{
		Parsed: &event.MessageEventContent{MsgType: event.MsgNotice, Body: text},
	}, nil)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to send notice to management room")
	}
}
