package connector

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
)

// maxPins bounds how many pinned messages are mirrored.
const maxPins = 100

// syncPins mirrors a channel's pinned messages into m.room.pinned_events.
// bridgev2 has no pin support, so the bridge bot sets the state directly.
// Pins of messages that were never bridged are skipped.
func (c *ChattoClient) syncPins(ctx context.Context, roomID string) {
	log := zerolog.Ctx(ctx).With().Str("room_id", roomID).Logger()
	portal, err := c.Main.Bridge.DB.Portal.GetByKey(ctx, c.portalKey(roomID))
	if err != nil || portal == nil || portal.MXID == "" {
		return
	}
	resp, err := c.api.Rooms.ListPinnedMessages(ctx, connect.NewRequest(&apiv1.ListPinnedMessagesRequest{
		RoomId: roomID,
		Page:   &apiv1.PageRequest{Limit: maxPins},
	}))
	if err != nil {
		log.Warn().Err(err).Msg("Failed to list pinned messages")
		return
	}
	pinned := make([]id.EventID, 0, len(resp.Msg.GetPinnedMessages()))
	for _, pm := range resp.Msg.GetPinnedMessages() {
		msg, err := c.Main.Bridge.DB.Message.GetFirstPartByID(ctx, "", c.messageID(pm.GetMessage().GetId()))
		if err != nil || msg == nil {
			continue
		}
		pinned = append(pinned, msg.MXID)
	}
	_, err = c.Main.Bridge.Bot.SendState(ctx, portal.MXID, event.StatePinnedEvents, "", &event.Content{
		Parsed: &event.PinnedEventsEventContent{Pinned: pinned},
	}, time.Now())
	if err != nil {
		log.Warn().Err(err).Msg("Failed to update pinned events")
	}
}
