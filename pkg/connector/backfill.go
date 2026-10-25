package connector

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/emoji"
)

// maxForwardPages bounds forward fill after a long outage.
const maxForwardPages = 20

func (c *ChattoClient) FetchMessages(ctx context.Context, params bridgev2.FetchMessagesParams) (*bridgev2.FetchMessagesResponse, error) {
	roomID, ok := chattoRoomID(params.Portal)
	if !ok {
		// Spaces have no timeline.
		return &bridgev2.FetchMessagesResponse{HasMore: false}, nil
	}
	var err error
	limit := int32(min(max(params.Count, 1), 500))
	threadRoot := chattoid.ParseMessageID(params.ThreadRoot)
	pager := &timelinePager{c: c, roomID: roomID, threadRoot: threadRoot, limit: limit}

	var events []*apiv1.RoomTimelineEvent
	var page *apiv1.RoomTimelinePage
	if params.Forward {
		if params.AnchorMessage == nil {
			page, err = pager.latest(ctx)
			if err != nil {
				return nil, err
			}
			events = page.GetEvents()
		} else {
			anchorID := chattoid.ParseMessageID(params.AnchorMessage.ID)
			page, err = pager.around(ctx, anchorID)
			if err != nil {
				return nil, err
			}
			events = eventsAfter(page.GetEvents(), anchorID)
			for i := 0; page.GetHasNewer() && i < maxForwardPages && len(events) < params.Count; i++ {
				page, err = pager.after(ctx, page.GetEndCursor())
				if err != nil {
					return nil, err
				}
				events = append(events, page.GetEvents()...)
			}
		}
	} else {
		switch {
		case params.Cursor != "":
			page, err = pager.before(ctx, string(params.Cursor))
			if err == nil {
				events = page.GetEvents()
			}
		case params.AnchorMessage != nil:
			anchorID := chattoid.ParseMessageID(params.AnchorMessage.ID)
			page, err = pager.around(ctx, anchorID)
			if err == nil {
				events = eventsBefore(page.GetEvents(), anchorID)
			}
		default:
			page, err = pager.latest(ctx)
			if err == nil {
				events = page.GetEvents()
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if users := page.GetIncludes().GetUsers(); len(users) > 0 {
		list := make([]*apiv1.User, 0, len(users))
		for _, u := range users {
			list = append(list, u)
		}
		c.users.Put(list...)
	}
	resp := &bridgev2.FetchMessagesResponse{
		Forward:  params.Forward,
		HasMore:  !params.Forward && page.GetHasOlder(),
		Cursor:   networkid.PaginationCursor(page.GetStartCursor()),
		MarkRead: false,
		// Thread replies and echoes can arrive out of the room's order.
		AggressiveDeduplication: params.Forward,
	}
	for _, evt := range events {
		if bm := c.backfillMessage(ctx, params.Portal, roomID, threadRoot, evt); bm != nil {
			resp.Messages = append(resp.Messages, bm)
		}
	}
	return resp, nil
}

// timelinePager pages either a room timeline or one thread.
type timelinePager struct {
	c          *ChattoClient
	roomID     string
	threadRoot string
	limit      int32
}

func (p *timelinePager) latest(ctx context.Context) (*apiv1.RoomTimelinePage, error) {
	if p.threadRoot != "" {
		resp, err := p.c.api.Threads.GetThreadEvents(ctx, connect.NewRequest(&apiv1.GetThreadEventsRequest{
			RoomId: p.roomID, ThreadRootEventId: p.threadRoot, Limit: p.limit,
		}))
		if err != nil {
			return nil, fmt.Errorf("failed to fetch thread: %w", err)
		}
		return resp.Msg.GetPage(), nil
	}
	resp, err := p.c.api.Rooms.GetRoomEvents(ctx, connect.NewRequest(&apiv1.GetRoomEventsRequest{RoomId: p.roomID, Limit: p.limit}))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch room timeline: %w", err)
	}
	return resp.Msg.GetPage(), nil
}

func (p *timelinePager) before(ctx context.Context, cursor string) (*apiv1.RoomTimelinePage, error) {
	if p.threadRoot != "" {
		resp, err := p.c.api.Threads.GetThreadEvents(ctx, connect.NewRequest(&apiv1.GetThreadEventsRequest{
			RoomId: p.roomID, ThreadRootEventId: p.threadRoot, Limit: p.limit,
			Cursor: &apiv1.GetThreadEventsRequest_Before{Before: cursor},
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetPage(), nil
	}
	resp, err := p.c.api.Rooms.GetRoomEvents(ctx, connect.NewRequest(&apiv1.GetRoomEventsRequest{
		RoomId: p.roomID, Limit: p.limit, Cursor: &apiv1.GetRoomEventsRequest_Before{Before: cursor},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPage(), nil
}

func (p *timelinePager) after(ctx context.Context, cursor string) (*apiv1.RoomTimelinePage, error) {
	if p.threadRoot != "" {
		resp, err := p.c.api.Threads.GetThreadEvents(ctx, connect.NewRequest(&apiv1.GetThreadEventsRequest{
			RoomId: p.roomID, ThreadRootEventId: p.threadRoot, Limit: p.limit,
			Cursor: &apiv1.GetThreadEventsRequest_After{After: cursor},
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetPage(), nil
	}
	resp, err := p.c.api.Rooms.GetRoomEvents(ctx, connect.NewRequest(&apiv1.GetRoomEventsRequest{
		RoomId: p.roomID, Limit: p.limit, Cursor: &apiv1.GetRoomEventsRequest_After{After: cursor},
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPage(), nil
}

func (p *timelinePager) around(ctx context.Context, eventID string) (*apiv1.RoomTimelinePage, error) {
	if p.threadRoot != "" {
		resp, err := p.c.api.Threads.GetThreadEventsAround(ctx, connect.NewRequest(&apiv1.GetThreadEventsAroundRequest{
			RoomId: p.roomID, ThreadRootEventId: p.threadRoot, EventId: eventID, Limit: p.limit,
		}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetPage(), nil
	}
	resp, err := p.c.api.Rooms.GetRoomEventsAround(ctx, connect.NewRequest(&apiv1.GetRoomEventsAroundRequest{
		RoomId: p.roomID, EventId: eventID, Limit: p.limit,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetPage(), nil
}

func eventsAfter(events []*apiv1.RoomTimelineEvent, anchorID string) []*apiv1.RoomTimelineEvent {
	for i, evt := range events {
		if evt.GetId() == anchorID {
			return events[i+1:]
		}
	}
	return events
}

func eventsBefore(events []*apiv1.RoomTimelineEvent, anchorID string) []*apiv1.RoomTimelineEvent {
	for i, evt := range events {
		if evt.GetId() == anchorID {
			return events[:i]
		}
	}
	return events
}

// backfillMessage converts one timeline event, or returns nil for events
// that are not bridged as messages.
func (c *ChattoClient) backfillMessage(ctx context.Context, portal *bridgev2.Portal, roomID, threadRoot string, evt *apiv1.RoomTimelineEvent) *bridgev2.BackfillMessage {
	posted := evt.GetMessagePosted()
	if posted == nil {
		return nil
	}
	msg := posted.GetMessage()
	if msg.GetEchoOfEventId() != "" || msg.GetDeletedAt() != nil {
		return nil
	}
	if threadRoot != "" && msg.GetId() == threadRoot {
		// The thread page repeats its root; it is bridged with the room.
		return nil
	}
	intent, ok := portal.GetIntentFor(ctx, c.makeEventSender(msg.GetActorId()), c.UserLogin, bridgev2.RemoteEventBackfill)
	if !ok {
		return nil
	}
	bm := &bridgev2.BackfillMessage{
		ConvertedMessage: c.convertMessage(ctx, portal, intent, msg),
		Sender:           c.makeEventSender(msg.GetActorId()),
		ID:               chattoid.MakeMessageID(msg.GetId()),
		Timestamp:        msg.GetCreatedAt().AsTime(),
		Reactions:        c.backfillReactions(ctx, roomID, msg),
	}
	if threadRoot == "" && msg.GetThread().GetReplyCount() > 0 {
		bm.ShouldBackfillThread = true
	}
	return bm
}

// backfillReactions expands aggregated reactions into individual ones. The
// message only carries up to five users per emoji, so larger counts are
// listed explicitly.
func (c *ChattoClient) backfillReactions(ctx context.Context, roomID string, msg *apiv1.Message) []*bridgev2.BackfillReaction {
	var out []*bridgev2.BackfillReaction
	for _, r := range msg.GetReactions() {
		users := r.GetPreviewUserIds()
		if int(r.GetCount()) > len(users) {
			listed, err := c.listReactionUsers(ctx, roomID, msg.GetId(), r.GetEmoji())
			if err != nil {
				zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to list reaction users")
			} else {
				users = listed
			}
		}
		for _, uid := range users {
			out = append(out, &bridgev2.BackfillReaction{
				Sender:  c.makeEventSender(uid),
				EmojiID: chattoid.MakeEmojiID(r.GetEmoji()),
				Emoji:   emoji.FromShortcode(r.GetEmoji()),
			})
		}
	}
	return out
}

func (c *ChattoClient) listReactionUsers(ctx context.Context, roomID, messageID, shortcode string) ([]string, error) {
	var all []string
	offset := int32(0)
	for {
		resp, err := c.api.Messages.ListReactionUsers(ctx, connect.NewRequest(&apiv1.ListReactionUsersRequest{
			RoomId: roomID, MessageEventId: messageID, Emoji: shortcode,
			Page: &apiv1.PageRequest{Limit: 100, Offset: offset},
		}))
		if err != nil {
			return nil, err
		}
		all = append(all, resp.Msg.GetUserIds()...)
		if !resp.Msg.GetPage().GetHasMore() || len(resp.Msg.GetUserIds()) == 0 || len(all) >= 1000 {
			return all, nil
		}
		offset += int32(len(resp.Msg.GetUserIds()))
	}
}
