package connector

import (
	"context"
	"time"

	"go.mau.fi/util/jsontime"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/event"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
)

const (
	// maxBodyLength is Chatto's message body limit in bytes.
	maxBodyLength = 10000
	// defaultMaxUploadSize is Chatto's default core.assets.max_upload_size.
	defaultMaxUploadSize = 25 << 20
	capabilityID         = "run.chatto.capabilities.2026_10_26"
)

func fileFeatures(maxSize int64) *event.FileFeatures {
	return &event.FileFeatures{
		MimeTypes:        map[string]event.CapabilitySupportLevel{"*/*": event.CapLevelFullySupported},
		MaxSize:          maxSize,
		Caption:          event.CapLevelFullySupported,
		MaxCaptionLength: maxBodyLength,
	}
}

func baseCapabilities(maxSize int64) *event.RoomFeatures {
	return &event.RoomFeatures{
		ID: capabilityID,
		Formatting: event.FormattingFeatureMap{
			event.FmtBold:               event.CapLevelFullySupported,
			event.FmtItalic:             event.CapLevelFullySupported,
			event.FmtStrikethrough:      event.CapLevelFullySupported,
			event.FmtInlineCode:         event.CapLevelFullySupported,
			event.FmtCodeBlock:          event.CapLevelFullySupported,
			event.FmtSyntaxHighlighting: event.CapLevelFullySupported,
			event.FmtBlockquote:         event.CapLevelFullySupported,
			event.FmtInlineLink:         event.CapLevelFullySupported,
			event.FmtUserLink:           event.CapLevelFullySupported,
			event.FmtUnorderedList:      event.CapLevelFullySupported,
			event.FmtOrderedList:        event.CapLevelFullySupported,
			event.FmtListStart:          event.CapLevelFullySupported,
			event.FmtHeaders:            event.CapLevelFullySupported,
			event.FmtTable:              event.CapLevelFullySupported,
			event.FmtUnderline:          event.CapLevelDropped,
			event.FmtSpoiler:            event.CapLevelDropped,
			event.FmtHorizontalLine:     event.CapLevelPartialSupport,
		},
		File: event.FileFeatureMap{
			event.MsgImage:      fileFeatures(maxSize),
			event.MsgVideo:      fileFeatures(maxSize),
			event.MsgAudio:      fileFeatures(maxSize),
			event.MsgFile:       fileFeatures(maxSize),
			event.CapMsgVoice:   fileFeatures(maxSize),
			event.CapMsgGIF:     fileFeatures(maxSize),
			event.CapMsgSticker: fileFeatures(maxSize),
		},
		MaxTextLength:        maxBodyLength,
		LocationMessage:      event.CapLevelPartialSupport,
		Reply:                event.CapLevelFullySupported,
		Thread:               event.CapLevelFullySupported,
		Edit:                 event.CapLevelFullySupported,
		EditMaxAge:           ptr.Ptr(jsontime.S(3 * time.Hour)),
		Delete:               event.CapLevelFullySupported,
		Reaction:             event.CapLevelFullySupported,
		ReactionCount:        20,
		CustomEmojiReactions: false,
		ReadReceipts:         true,
		TypingNotifications:  true,
	}
}

func (c *ChattoClient) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	caps := baseCapabilities(min(c.maxUploadSize(ctx), maxFileSize))
	meta, _ := portal.Metadata.(*chattoid.PortalMetadata)
	if meta == nil {
		return caps
	}
	switch {
	case meta.Kind == portalKindDM || meta.Kind == portalKindGroupDM:
		caps.ID += "+dm"
		caps.Thread = event.CapLevelRejected
	case meta.ThreadingMode == int32(apiv1.RoomThreadingMode_ROOM_THREADING_MODE_DISABLED):
		caps.ID += "+nothreads"
		caps.Thread = event.CapLevelRejected
	}
	if meta.Kind == portalKindChannel && c.Main.Config.GhostBots.Enabled && !c.isBot() {
		// Relayed messages arrive unformatted and are posted by ghost bots.
		caps.ID += "+ghostbots"
		caps.PerMessageProfileRelay = true
	}
	if meta.Kind == portalKindChannel {
		caps.State = event.StateFeatureMap{
			event.StateRoomName.Type: {Level: event.CapLevelFullySupported},
			event.StateTopic.Type:    {Level: event.CapLevelFullySupported},
		}
	}
	return caps
}
