package connector

import (
	"context"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/matrix"
	"maunium.net/go/mautrix/event"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/emoji"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
)

// presenceSetter is the part of an appservice intent needed to set presence.
// bridgev2 has no presence API, so this reaches into the Matrix connector.
type presenceSetter interface {
	SetPresence(ctx context.Context, presence mautrix.ReqPresence) error
}

// presenceFromChatto maps a Chatto presence to Matrix presence.
func presenceFromChatto(status apiv1.PresenceStatus) event.Presence {
	switch status {
	case apiv1.PresenceStatus_PRESENCE_STATUS_ONLINE:
		return event.PresenceOnline
	case apiv1.PresenceStatus_PRESENCE_STATUS_AWAY, apiv1.PresenceStatus_PRESENCE_STATUS_DO_NOT_DISTURB:
		return event.PresenceUnavailable
	default:
		return event.PresenceOffline
	}
}

// statusMessage renders a Chatto custom status (and DND, which Matrix lacks)
// as a Matrix status message.
func statusMessage(status apiv1.PresenceStatus, custom *apiv1.CustomUserStatus, now time.Time) string {
	var parts []string
	if status == apiv1.PresenceStatus_PRESENCE_STATUS_DO_NOT_DISTURB {
		parts = append(parts, "⛔ Do not disturb")
	}
	if custom != nil && (custom.GetExpiresAt() == nil || custom.GetExpiresAt().AsTime().After(now)) {
		text := strings.TrimSpace(custom.GetEmoji() + " " + custom.GetText())
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " · ")
}

func (c *ChattoClient) setGhostPresence(ctx context.Context, ghost *bridgev2.Ghost, status apiv1.PresenceStatus, custom *apiv1.CustomUserStatus) {
	var setter presenceSetter
	switch intent := ghost.Intent.(type) {
	case *matrix.ASIntent:
		if err := intent.Matrix.EnsureRegistered(ctx); err != nil {
			zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to register ghost for presence")
			return
		}
		setter = intent.Matrix
	case presenceSetter:
		setter = intent
	default:
		return
	}
	err := setter.SetPresence(ctx, mautrix.ReqPresence{
		Presence:  presenceFromChatto(status),
		StatusMsg: statusMessage(status, custom, time.Now()),
	})
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to set ghost presence")
	}
}

// emojiFromShortcode returns the Unicode emoji for a Chatto reaction name.
func emojiFromShortcode(name string) string {
	return emoji.FromShortcode(name)
}
