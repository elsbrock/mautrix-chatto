package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/mautrix-chatto/pkg/bridgemetrics"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
	"github.com/elsbrock/mautrix-chatto/pkg/chattofmt"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
	"github.com/elsbrock/mautrix-chatto/pkg/emoji"
)

// idempotencyNamespace derives Chatto idempotency keys from Matrix event IDs,
// so a retried Matrix event never posts twice within Chatto's 30 minute window.
var idempotencyNamespace = uuid.MustParse("6f1b3b8e-6c43-4f0e-9d1c-5f1e0b2f7a10")

func idempotencyKey(evtID id.EventID) string {
	return uuid.NewSHA1(idempotencyNamespace, []byte(evtID)).String()
}

var (
	ErrGroupPortal = bridgev2.WrapErrorInStatus(errors.New("room groups are spaces and have no messages")).
			WithErrorAsMessage().WithIsCertain(true).WithSendNotice(false)
	ErrUnsupportedReaction = bridgev2.WrapErrorInStatus(errors.New("this emoji can't be used as a reaction on Chatto")).
				WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
	ErrMessageTooLong = bridgev2.WrapErrorInStatus(fmt.Errorf("message is longer than Chatto's %d byte limit", maxBodyLength)).
				WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
	ErrNotChattoMessage = bridgev2.WrapErrorInStatus(errors.New("bridge notices only exist on Matrix")).
				WithErrorAsMessage().WithIsCertain(true).WithSendNotice(false)
	ErrRoomArchived = bridgev2.WrapErrorInStatus(errors.New("this room is archived on Chatto")).
			WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
	// ErrServerMismatch is returned when bridgev2 picks a login (relay,
	// default login) from another Chatto server than the room's.
	ErrServerMismatch = bridgev2.WrapErrorInStatus(errServerMismatch).
				WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
)

// isChattoMessageID reports whether a message exists on Chatto, as opposed
// to bridge-generated notices (call, archive) that only exist on Matrix.
func isChattoMessageID(id networkid.MessageID) bool {
	return !chattoid.IsNoticeMessageID(id)
}

// portalRoomID returns the Chatto room of a portal this login may act on.
// Spaces have no room, and rooms of other servers are refused.
func (c *ChattoClient) portalRoomID(portal *bridgev2.Portal) (string, error) {
	srv, kind, roomID := chattoid.ParsePortalID(portal.ID)
	switch {
	case kind != chattoid.PortalRoom:
		return "", ErrGroupPortal
	case srv != c.serverID:
		return "", ErrServerMismatch
	}
	return roomID, nil
}

// chattoRoomID is portalRoomID for callers that ignore spaces and foreign
// rooms silently.
func (c *ChattoClient) chattoRoomID(portal *bridgev2.Portal) (string, bool) {
	roomID, err := c.portalRoomID(portal)
	return roomID, err == nil
}

// eventID returns the Chatto event ID of a message on this login's server,
// or "" for notices and other servers' messages.
func (c *ChattoClient) eventID(id networkid.MessageID) string {
	eventID, _ := c.chattoEventID(id)
	return eventID
}

func portalMeta(portal *bridgev2.Portal) *chattoid.PortalMetadata {
	meta, _ := portal.Metadata.(*chattoid.PortalMetadata)
	if meta == nil {
		return &chattoid.PortalMetadata{}
	}
	return meta
}

func (c *ChattoClient) fromMatrixOptions() *chattofmt.FromMatrixOptions {
	return &chattofmt.FromMatrixOptions{
		ResolveUser: func(ctx context.Context, userID id.UserID) (string, bool) {
			return c.chattoLoginForMatrixUser(ctx, userID)
		},
		ResolveEventLink: func(ctx context.Context, roomID id.RoomID, eventID id.EventID) (string, bool) {
			msg, err := c.Main.Bridge.DB.Message.GetPartByMXID(ctx, eventID)
			if err != nil || msg == nil {
				return "", false
			}
			srvID, chattoRoom := chattoid.ParseRoomPortalID(msg.Room.ID)
			_, chattoEvent := chattoid.ParseMessageID(msg.ID)
			srv := c.Main.server(srvID)
			if srv == nil || chattoRoom == "" || chattoEvent == "" {
				return "", false
			}
			base, err := chattoclient.ParseServerURL(srv.URL)
			if err != nil {
				return "", false
			}
			return fmt.Sprintf("%s/chat/-/%s/m/%s", base, chattoRoom, chattoEvent), true
		},
	}
}

// chattoLoginForMatrixUser maps a Matrix user to a Chatto login: ghosts map
// to their Chatto user, real users to the Chatto account they logged in with.
func (c *ChattoClient) chattoLoginForMatrixUser(ctx context.Context, userID id.UserID) (string, bool) {
	if ghostID, ok := c.Main.Bridge.Matrix.ParseGhostMXID(userID); ok {
		chattoUserID, ok := c.chattoUserID(ghostID)
		if !ok {
			return "", false
		}
		user, err := c.users.Get(ctx, chattoUserID)
		if err != nil {
			return "", false
		}
		return user.GetLogin(), true
	}
	user, err := c.Main.Bridge.GetExistingUserByMXID(ctx, userID)
	if err != nil || user == nil {
		return "", false
	}
	for _, login := range user.GetUserLogins() {
		if srv, _ := chattoid.ParseUserLoginID(login.ID); srv != c.serverID {
			continue
		}
		if meta, ok := login.Metadata.(*chattoid.UserLoginMetadata); ok && meta.Login != "" {
			return meta.Login, true
		}
	}
	return "", false
}

// matrixToChattoBody converts message text, handling emotes and locations.
func (c *ChattoClient) matrixToChattoBody(ctx context.Context, content *event.MessageEventContent) string {
	body := chattofmt.FromMatrix(ctx, content, c.fromMatrixOptions())
	switch content.MsgType {
	case event.MsgEmote:
		body = "*" + strings.TrimSpace(body) + "*"
	case event.MsgLocation:
		if body == "" {
			body = "Location"
		}
		if content.GeoURI != "" {
			body = fmt.Sprintf("📍 %s (%s)", body, content.GeoURI)
		}
	}
	return body
}

func (c *ChattoClient) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	roomID, err := c.portalRoomID(msg.Portal)
	if err != nil {
		return nil, err
	}
	meta := portalMeta(msg.Portal)
	if meta.Archived {
		return nil, ErrRoomArchived
	}
	req := &apiv1.CreateMessageRequest{RoomId: roomID}
	key := idempotencyKey(msg.Event.ID)
	req.IdempotencyKey = &key

	threadsAllowed := meta.Kind == portalKindChannel &&
		meta.ThreadingMode != int32(apiv1.RoomThreadingMode_ROOM_THREADING_MODE_DISABLED)
	if msg.ThreadRoot != nil {
		rootID := c.eventID(msg.ThreadRoot.ID)
		if threadsAllowed {
			req.ThreadRootEventId = rootID
		} else {
			req.InReplyTo = rootID
		}
	}
	if msg.ReplyTo != nil && isChattoMessageID(msg.ReplyTo.ID) {
		replyID := c.eventID(msg.ReplyTo.ID)
		// In a REQUIRED-threading room, replying to a root means replying in its thread.
		if threadsAllowed && req.ThreadRootEventId == "" && msg.ReplyTo.ThreadRoot == "" &&
			meta.ThreadingMode == int32(apiv1.RoomThreadingMode_ROOM_THREADING_MODE_REQUIRED) {
			req.ThreadRootEventId = replyID
		} else {
			req.InReplyTo = replyID
		}
	}

	api := c.api
	senderID := c.userID
	content := msg.Content
	if msg.OrigSender != nil && c.Main.Config.GhostBots.Enabled {
		bot, err := c.ensureGhostBot(ctx, msg.OrigSender, roomID)
		if err == nil {
			api = c.ghostBotClient(bot)
			senderID = bot.UserID
		} else {
			// Fall back to plain relaying with a name prefix.
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Ghost bot unavailable, relaying with a name prefix")
			formatted, ferr := c.Main.Bridge.Config.Relay.FormatMessage(content, msg.OrigSender)
			if ferr != nil {
				return nil, ferr
			}
			content = formatted
		}
	}
	msgType := "text"
	switch content.MsgType {
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		msgType = "media"
		assetID, err := c.uploadMatrixMedia(ctx, api, roomID, content)
		if err != nil {
			return nil, err
		}
		req.AttachmentAssetIds = []string{assetID}
		if caption := mediaCaption(content); caption != "" {
			req.Body = c.matrixToChattoBody(ctx, captionContent(content))
		}
	default:
		if msg.Event.Type == event.EventSticker {
			msgType = "sticker"
			assetID, err := c.uploadMatrixMedia(ctx, api, roomID, content)
			if err != nil {
				return nil, err
			}
			req.AttachmentAssetIds = []string{assetID}
			break
		}
		req.Body = c.matrixToChattoBody(ctx, content)
	}
	if len(req.Body) > maxBodyLength {
		return nil, ErrMessageTooLong
	}

	finish := c.Main.echoes.Begin(string(c.portalKey(roomID).ID))
	resp, err := api.Messages.CreateMessage(ctx, connect.NewRequest(req))
	if err != nil {
		finish("")
		bridgemetrics.BridgeErrors.WithLabelValues("to_chatto", msgType).Inc()
		return nil, wrapSendError(err)
	}
	created := resp.Msg.GetMessage()
	finish(string(c.messageID(created.GetId())))
	bridgemetrics.MessagesBridged.WithLabelValues("to_chatto", msgType).Inc()
	dbMsg := &database.Message{
		ID:        c.messageID(created.GetId()),
		SenderID:  c.ghostID(senderID),
		Timestamp: created.GetCreatedAt().AsTime(),
		Metadata:  &chattoid.MessageMetadata{TextHash: chattoid.HashText(req.Body)},
	}
	if len(req.AttachmentAssetIds) > 0 && len(created.GetAttachments()) == 1 {
		// A Matrix media event (captioned or not) maps to the Chatto
		// attachment part, so later Chatto edits never turn it into text
		// or duplicate the file.
		dbMsg.PartID = chattoid.MakeAttachmentPartID(created.GetAttachments()[0].GetId())
		dbMsg.Metadata = &chattoid.MessageMetadata{AssetID: created.GetAttachments()[0].GetId()}
	}
	if created.GetThreadRootEventId() != "" && created.GetThreadRootEventId() != created.GetId() {
		dbMsg.ThreadRoot = c.messageID(created.GetThreadRootEventId())
	}
	return &bridgev2.MatrixMessageResponse{DB: dbMsg}, nil
}

// mediaCaption returns the caption of a Matrix media message, if any.
func mediaCaption(content *event.MessageEventContent) string {
	if content.FileName != "" && content.Body != "" && content.Body != content.FileName {
		return content.Body
	}
	return ""
}

// captionContent returns a text-only copy of a captioned media message.
func captionContent(content *event.MessageEventContent) *event.MessageEventContent {
	return &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          content.Body,
		Format:        content.Format,
		FormattedBody: content.FormattedBody,
	}
}

// wrapSendError turns Chatto errors into user-facing message statuses.
func wrapSendError(err error) error {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return err
	}
	status := bridgev2.WrapErrorInStatus(err).WithErrorAsMessage().WithSendNotice(true)
	switch cerr.Code() {
	case connect.CodePermissionDenied:
		return bridgev2.WrapErrorInStatus(fmt.Errorf("chatto denied the message: %s", cerr.Message())).
			WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeAlreadyExists:
		return status.WithIsCertain(true)
	case connect.CodeResourceExhausted:
		return bridgev2.WrapErrorInStatus(fmt.Errorf("chatto rate limit or slow mode: %s", cerr.Message())).
			WithErrorAsMessage().WithIsCertain(true).WithSendNotice(true)
	}
	return err
}

func (c *ChattoClient) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	roomID, err := c.portalRoomID(msg.Portal)
	if err != nil {
		return err
	}
	api := c.apiForMessage(ctx, msg.EditTarget)
	content := msg.Content
	if content.MsgType.IsMedia() {
		content = captionContent(content)
	}
	body := c.matrixToChattoBody(ctx, content)
	if len(body) > maxBodyLength {
		return ErrMessageTooLong
	}
	_, err = api.Messages.UpdateMessage(ctx, connect.NewRequest(&apiv1.UpdateMessageRequest{
		RoomId:     roomID,
		EventId:    c.eventID(msg.EditTarget.ID),
		Body:       &body,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"body"}},
	}))
	if err != nil {
		bridgemetrics.BridgeErrors.WithLabelValues("to_chatto", "edit").Inc()
		return wrapSendError(err)
	}
	bridgemetrics.MessagesBridged.WithLabelValues("to_chatto", "edit").Inc()
	// bridgev2 saves the edit target afterwards; recording the new text keeps
	// the realtime echo of this edit from being bridged back.
	if meta, ok := msg.EditTarget.Metadata.(*chattoid.MessageMetadata); ok {
		meta.TextHash = chattoid.HashText(body)
	}
	return nil
}

func (c *ChattoClient) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	roomID, err := c.portalRoomID(msg.Portal)
	if err != nil {
		return err
	}
	api := c.apiForMessage(ctx, msg.TargetMessage)
	eventID := c.eventID(msg.TargetMessage.ID)
	if attID, isAttachment := chattoid.ParseAttachmentPartID(msg.TargetMessage.PartID); isAttachment {
		// Redacting one attachment of a multi-part message removes just it.
		parts, err := c.Main.Bridge.DB.Message.GetAllPartsByID(ctx, msg.Portal.Receiver, msg.TargetMessage.ID)
		if err == nil && len(parts) > 1 {
			_, err = api.Messages.DeleteAttachment(ctx, connect.NewRequest(&apiv1.DeleteAttachmentRequest{
				RoomId: roomID, EventId: eventID, AttachmentId: attID,
			}))
			if err != nil {
				return wrapSendError(err)
			}
			bridgemetrics.MessagesBridged.WithLabelValues("to_chatto", "attachment_delete").Inc()
			return nil
		}
	}
	_, err = api.Messages.DeleteMessage(ctx, connect.NewRequest(&apiv1.DeleteMessageRequest{RoomId: roomID, EventId: eventID}))
	if err != nil {
		bridgemetrics.BridgeErrors.WithLabelValues("to_chatto", "redaction").Inc()
		return wrapSendError(err)
	}
	bridgemetrics.MessagesBridged.WithLabelValues("to_chatto", "redaction").Inc()
	return nil
}

func (c *ChattoClient) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	if !isChattoMessageID(msg.TargetMessage.ID) {
		return bridgev2.MatrixReactionPreResponse{}, ErrNotChattoMessage
	}
	shortcode, ok := emoji.ToShortcode(msg.Content.RelatesTo.Key)
	if !ok {
		return bridgev2.MatrixReactionPreResponse{}, ErrUnsupportedReaction
	}
	return bridgev2.MatrixReactionPreResponse{
		SenderID:     c.ghostID(c.userID),
		EmojiID:      chattoid.MakeEmojiID(shortcode),
		Emoji:        emoji.FromShortcode(shortcode),
		MaxReactions: 20,
	}, nil
}

func (c *ChattoClient) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	roomID, err := c.portalRoomID(msg.Portal)
	if err != nil {
		return nil, err
	}
	_, err = c.api.Messages.AddReaction(ctx, connect.NewRequest(&apiv1.AddReactionRequest{
		RoomId:         roomID,
		MessageEventId: c.eventID(msg.TargetMessage.ID),
		Emoji:          string(msg.PreHandleResp.EmojiID),
	}))
	if err != nil {
		bridgemetrics.BridgeErrors.WithLabelValues("to_chatto", "reaction").Inc()
		return nil, wrapSendError(err)
	}
	bridgemetrics.MessagesBridged.WithLabelValues("to_chatto", "reaction").Inc()
	return &database.Reaction{}, nil
}

func (c *ChattoClient) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	roomID, err := c.portalRoomID(msg.Portal)
	if err != nil {
		return err
	}
	_, err = c.api.Messages.RemoveReaction(ctx, connect.NewRequest(&apiv1.RemoveReactionRequest{
		RoomId:         roomID,
		MessageEventId: c.eventID(msg.TargetReaction.MessageID),
		Emoji:          string(msg.TargetReaction.EmojiID),
	}))
	if err != nil {
		bridgemetrics.BridgeErrors.WithLabelValues("to_chatto", "reaction_remove").Inc()
		return wrapSendError(err)
	}
	bridgemetrics.MessagesBridged.WithLabelValues("to_chatto", "reaction_remove").Inc()
	return nil
}

func (c *ChattoClient) HandleMatrixReadReceipt(ctx context.Context, receipt *bridgev2.MatrixReadReceipt) error {
	roomID, ok := c.chattoRoomID(receipt.Portal)
	if !ok {
		return nil
	}
	var err error
	target := receipt.ExactMessage
	if target != nil && target.ThreadRoot != "" {
		_, err = c.api.Threads.MarkThreadAsRead(ctx, connect.NewRequest(&apiv1.MarkThreadAsReadRequest{
			RoomId:            roomID,
			ThreadRootEventId: c.eventID(target.ThreadRoot),
			UpToEventId:       c.eventID(target.ID),
		}))
		return err
	}
	req := &apiv1.MarkRoomAsReadRequest{RoomId: roomID}
	if target != nil && isChattoMessageID(target.ID) {
		req.UpToEventId = c.eventID(target.ID)
	}
	_, err = c.api.Rooms.MarkRoomAsRead(ctx, connect.NewRequest(req))
	if req.UpToEventId != "" && (connect.CodeOf(err) == connect.CodeInvalidArgument || connect.CodeOf(err) == connect.CodeNotFound) {
		// Thread echoes are not root messages; mark through the latest root.
		req.UpToEventId = ""
		_, err = c.api.Rooms.MarkRoomAsRead(ctx, connect.NewRequest(req))
	}
	return err
}

// typingRefreshInterval matches Chatto's client: refresh at most every 2 s.
const typingRefreshInterval = 2 * time.Second

// maxTypingDuration stops refreshing if Matrix never sends a stop.
const maxTypingDuration = 2 * time.Minute

func (c *ChattoClient) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	roomID, ok := c.chattoRoomID(msg.Portal)
	if !ok {
		return nil
	}
	c.typingLock.Lock()
	defer c.typingLock.Unlock()
	if cancel, ok := c.typing[msg.Portal.ID]; ok {
		cancel()
		delete(c.typing, msg.Portal.ID)
	}
	if !msg.IsTyping {
		// Chatto has no stop call; receivers expire the indicator after 6 s.
		return nil
	}
	typingCtx, cancel := context.WithTimeout(c.UserLogin.Log.WithContext(c.Main.Bridge.BackgroundCtx), maxTypingDuration)
	c.typing[msg.Portal.ID] = cancel
	go func() {
		ticker := time.NewTicker(typingRefreshInterval)
		defer ticker.Stop()
		for {
			_, err := c.api.Rooms.RefreshTypingIndicator(typingCtx, connect.NewRequest(&apiv1.RefreshTypingIndicatorRequest{RoomId: roomID}))
			if err != nil && typingCtx.Err() == nil {
				zerolog.Ctx(typingCtx).Debug().Err(err).Msg("Failed to send typing indicator")
				return
			}
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}

func (c *ChattoClient) HandleMatrixRoomName(ctx context.Context, msg *bridgev2.MatrixRoomName) (bool, error) {
	roomID, ok := c.chattoRoomID(msg.Portal)
	if !ok || portalMeta(msg.Portal).Kind != portalKindChannel {
		return false, nil
	}
	name := strings.TrimSpace(msg.Content.Name)
	if name == "" || len([]rune(name)) > 30 {
		return false, bridgev2.WrapErrorInStatus(errors.New("chatto room names must be 1-30 characters")).WithIsCertain(true).WithErrorAsMessage()
	}
	_, err := c.api.Rooms.UpdateRoom(ctx, connect.NewRequest(&apiv1.UpdateRoomRequest{
		RoomId:     roomID,
		Name:       &name,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}},
	}))
	if err != nil {
		return false, wrapSendError(err)
	}
	return true, nil
}

func (c *ChattoClient) HandleMatrixRoomTopic(ctx context.Context, msg *bridgev2.MatrixRoomTopic) (bool, error) {
	roomID, ok := c.chattoRoomID(msg.Portal)
	if !ok || portalMeta(msg.Portal).Kind != portalKindChannel {
		return false, nil
	}
	topic := msg.Content.Topic
	if len([]rune(topic)) > 500 {
		return false, bridgev2.WrapErrorInStatus(errors.New("chatto room descriptions are limited to 500 characters")).WithIsCertain(true).WithErrorAsMessage()
	}
	_, err := c.api.Rooms.UpdateRoom(ctx, connect.NewRequest(&apiv1.UpdateRoomRequest{
		RoomId:      roomID,
		Description: &topic,
		UpdateMask:  &fieldmaskpb.FieldMask{Paths: []string{"description"}},
	}))
	if err != nil {
		return false, wrapSendError(err)
	}
	return true, nil
}

func (c *ChattoClient) HandleMatrixMembership(ctx context.Context, msg *bridgev2.MatrixMembershipChange) (*bridgev2.MatrixMembershipResult, error) {
	roomID, err := c.portalRoomID(msg.Portal)
	if err != nil {
		return nil, err
	}
	meta := portalMeta(msg.Portal)
	switch msg.Type {
	case bridgev2.Leave:
		if meta.Universal || meta.Kind != portalKindChannel {
			// Universal channels and DMs cannot be left on Chatto.
			return nil, nil
		}
		_, err = c.api.Rooms.LeaveRoom(ctx, connect.NewRequest(&apiv1.LeaveRoomRequest{RoomId: roomID}))
		return nil, wrapSendError(err)
	case bridgev2.Invite:
		targetID, ok := c.membershipTargetUserID(msg.Target)
		if !ok || meta.Kind != portalKindChannel {
			return nil, errors.New("only Chatto users can be invited to Chatto channels")
		}
		_, err = c.api.Rooms.AddMember(ctx, connect.NewRequest(&apiv1.AddMemberRequest{RoomId: roomID, UserId: targetID}))
		if err != nil {
			return nil, wrapSendError(err)
		}
		return &bridgev2.MatrixMembershipResult{}, nil
	case bridgev2.Kick:
		targetID, ok := c.membershipTargetUserID(msg.Target)
		if !ok || meta.Kind != portalKindChannel {
			return nil, nil
		}
		_, err = c.api.Rooms.RemoveMember(ctx, connect.NewRequest(&apiv1.RemoveMemberRequest{RoomId: roomID, UserId: targetID}))
		return nil, wrapSendError(err)
	}
	return nil, nil
}

// membershipTargetUserID returns the Chatto user of a membership target on
// this login's server.
func (c *ChattoClient) membershipTargetUserID(target bridgev2.GhostOrUserLogin) (string, bool) {
	switch t := target.(type) {
	case *bridgev2.Ghost:
		return c.chattoUserID(t.ID)
	case *bridgev2.UserLogin:
		srv, userID := chattoid.ParseUserLoginID(t.ID)
		return userID, srv == c.serverID && userID != ""
	}
	return "", false
}

// apiForMessage returns the client that may edit or delete a bridged
// message: the ghost bot that sent it, or this login.
func (c *ChattoClient) apiForMessage(ctx context.Context, msg *database.Message) *chattoclient.Client {
	if msg != nil {
		if bot := c.Main.ghostBotForMessage(ctx, msg.SenderID); bot != nil {
			return c.ghostBotClient(bot)
		}
	}
	return c.api
}
