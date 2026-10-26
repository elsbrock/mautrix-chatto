package connector

import (
	"context"
	"fmt"
	"mime"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattofmt"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
)

// maxFileSize is the homeserver's upload limit, set by bridgev2.
var maxFileSize int64 = 100 << 20

// matrixUserForChatto returns the Matrix user that represents a Chatto user:
// the real Matrix account if that Chatto user is logged into the bridge,
// otherwise their ghost.
func (c *ChattoClient) matrixUserForChatto(ctx context.Context, chattoUserID string) id.UserID {
	if ul := c.Main.Bridge.GetCachedUserLoginByID(chattoid.MakeUserLoginID(c.serverID, chattoUserID)); ul != nil {
		return ul.UserMXID
	}
	return c.Main.Bridge.Matrix.GhostIntent(c.ghostID(chattoUserID)).GetMXID()
}

func (c *ChattoClient) toMatrixOptions() *chattofmt.ToMatrixOptions {
	return &chattofmt.ToMatrixOptions{
		ServerURL: c.api.BaseURL().String(),
		ResolveMention: func(ctx context.Context, handle string) (chattofmt.MentionTarget, bool) {
			user, ok := c.users.GetByLogin(ctx, handle)
			if !ok {
				return chattofmt.MentionTarget{}, false
			}
			name := c.Main.Config.FormatDisplayname(DisplaynameParams{
				DisplayName: user.GetDisplayName(), Login: user.GetLogin(), ID: user.GetId(), IsBot: user.GetBot() != nil,
			})
			return chattofmt.MentionTarget{UserID: string(c.matrixUserForChatto(ctx, user.GetId())), DisplayName: name}, true
		},
	}
}

// convertText converts a Chatto body to Matrix content, including @all and
// @here as room mentions.
func (c *ChattoClient) convertText(ctx context.Context, body string) *event.MessageEventContent {
	content := chattofmt.ToMatrix(ctx, body, c.toMatrixOptions())
	for _, handle := range chattofmt.ExtractMentions(body) {
		if h := strings.ToLower(handle); h == "all" || h == "here" {
			if content.Mentions == nil {
				content.Mentions = &event.Mentions{}
			}
			content.Mentions.Room = true
		}
	}
	return content
}

// convertMessage converts a Chatto message into Matrix parts: the text body
// first (part ID ""), then one part per attachment.
func (c *ChattoClient) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, msg *apiv1.Message) *bridgev2.ConvertedMessage {
	cm := &bridgev2.ConvertedMessage{}
	if root := msg.GetThreadRootEventId(); root != "" && root != msg.GetId() {
		cm.ThreadRoot = ptr.Ptr(c.messageID(root))
	}
	if reply := msg.GetInReplyTo(); reply != "" && reply != msg.GetThreadRootEventId() {
		cm.ReplyTo = &networkid.MessageOptionalPartID{MessageID: c.messageID(reply)}
	} else if reply != "" && cm.ThreadRoot == nil {
		cm.ReplyTo = &networkid.MessageOptionalPartID{MessageID: c.messageID(reply)}
	}
	if msg.GetDeletedAt() != nil {
		cm.Parts = append(cm.Parts, &bridgev2.ConvertedMessagePart{
			Type:    event.EventMessage,
			Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Message deleted"},
		})
		return cm
	}
	if body := msg.GetBody(); strings.TrimSpace(body) != "" || len(msg.GetAttachments()) == 0 {
		content := c.convertText(ctx, body)
		if lp := convertLinkPreview(msg.GetLinkPreview()); lp != nil {
			content.BeeperLinkPreviews = []*event.BeeperLinkPreview{lp}
		}
		cm.Parts = append(cm.Parts, &bridgev2.ConvertedMessagePart{
			Type:       event.EventMessage,
			Content:    content,
			DBMetadata: &chattoid.MessageMetadata{TextHash: chattoid.HashText(body)},
		})
	}
	for _, att := range msg.GetAttachments() {
		cm.Parts = append(cm.Parts, c.convertAttachment(ctx, portal, intent, att))
	}
	return cm
}

func convertLinkPreview(lp *apiv1.LinkPreview) *event.BeeperLinkPreview {
	if lp == nil || lp.GetUrl() == "" {
		return nil
	}
	return &event.BeeperLinkPreview{
		MatchedURL: lp.GetUrl(),
		LinkPreview: event.LinkPreview{
			CanonicalURL: lp.GetUrl(),
			Title:        lp.GetTitle(),
			Description:  lp.GetDescription(),
			SiteName:     lp.GetSiteName(),
		},
	}
}

func msgTypeForContentType(contentType string) event.MessageType {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(contentType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(contentType, "audio/"):
		return event.MsgAudio
	default:
		return event.MsgFile
	}
}

// convertAttachment downloads an attachment and reuploads it to Matrix. When
// the file is not available yet (video processing) or fails to transfer, a
// notice part stands in, marked so that a later asset event can replace it.
func (c *ChattoClient) convertAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, att *apiv1.MessageAttachment) *bridgev2.ConvertedMessagePart {
	partID := chattoid.MakeAttachmentPartID(att.GetId())
	filename := att.GetFilename()
	contentType := att.GetContentType()
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(filename))
	}
	meta := &chattoid.MessageMetadata{AssetID: att.GetId()}
	placeholder := func(text string) *bridgev2.ConvertedMessagePart {
		meta.Processing = true
		return &bridgev2.ConvertedMessagePart{
			ID:         partID,
			Type:       event.EventMessage,
			Content:    &event.MessageEventContent{MsgType: event.MsgNotice, Body: fmt.Sprintf("%s: %s", text, filename)},
			DBMetadata: meta,
		}
	}
	srcURL := att.GetAssetUrl().GetUrl()
	if vp := att.GetVideoProcessing(); vp != nil && srcURL == "" {
		switch vp.GetStatus() {
		case apiv1.MessageVideoProcessingStatus_MESSAGE_VIDEO_PROCESSING_STATUS_FAILED:
			meta.Processing = false
			return &bridgev2.ConvertedMessagePart{
				ID: partID, Type: event.EventMessage, DBMetadata: meta,
				Content: &event.MessageEventContent{MsgType: event.MsgNotice, Body: "Video processing failed on Chatto: " + filename},
			}
		default:
			return placeholder("Video is still processing on Chatto")
		}
	}
	if srcURL == "" {
		return placeholder("Attachment is not available yet")
	}
	data, err := c.downloadPublic(ctx, c.api.ResolveURL(srcURL), maxFileSize)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Str("attachment_id", att.GetId()).Msg("Failed to download Chatto attachment")
		return placeholder("Failed to bridge attachment")
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	content := &event.MessageEventContent{
		MsgType:  msgTypeForContentType(contentType),
		Body:     filename,
		FileName: filename,
		Info: &event.FileInfo{
			MimeType: contentType,
			Size:     len(data),
			Width:    int(att.GetWidth()),
			Height:   int(att.GetHeight()),
		},
	}
	if desc := att.GetDescription(); desc != "" {
		// Matrix has no alt text; use the description as the caption.
		content.Body = desc
	}
	if vp := att.GetVideoProcessing(); vp != nil && vp.GetDurationMs() > 0 {
		content.Info.Duration = int(vp.GetDurationMs())
	}
	var roomID id.RoomID
	if portal != nil {
		roomID = portal.MXID
	}
	content.URL, content.File, err = intent.UploadMedia(ctx, roomID, data, filename, contentType)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Str("attachment_id", att.GetId()).Msg("Failed to upload attachment to Matrix")
		return placeholder("Failed to bridge attachment")
	}
	return &bridgev2.ConvertedMessagePart{ID: partID, Type: event.EventMessage, Content: content, DBMetadata: meta}
}

// convertEdit maps an edited Chatto message onto the existing Matrix parts.
// Text changes edit the text part; attachments that finished processing
// replace their placeholder; removed attachments are redacted.
func (c *ChattoClient) convertEdit(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, msg *apiv1.Message) *bridgev2.ConvertedEdit {
	edit := &bridgev2.ConvertedEdit{}
	byPart := make(map[networkid.PartID]*database.Message, len(existing))
	for _, part := range existing {
		byPart[part.PartID] = part
	}
	textPart, hasText := byPart[""]
	if hasText {
		if meta, _ := textPart.Metadata.(*chattoid.MessageMetadata); meta != nil && meta.TextHash == chattoid.HashText(msg.GetBody()) {
			// Asset events and echoes of our own edits leave the text alone.
			hasText = false
		}
	}
	if hasText {
		content := c.convertText(ctx, msg.GetBody())
		if lp := convertLinkPreview(msg.GetLinkPreview()); lp != nil {
			content.BeeperLinkPreviews = []*event.BeeperLinkPreview{lp}
		}
		if meta, _ := textPart.Metadata.(*chattoid.MessageMetadata); meta != nil {
			meta.TextHash = chattoid.HashText(msg.GetBody())
		}
		edit.ModifiedParts = append(edit.ModifiedParts, &bridgev2.ConvertedEditPart{
			Part:    textPart,
			Type:    event.EventMessage,
			Content: content,
		})
	}
	seen := make(map[networkid.PartID]bool)
	var added []*bridgev2.ConvertedMessagePart
	for _, att := range msg.GetAttachments() {
		partID := chattoid.MakeAttachmentPartID(att.GetId())
		seen[partID] = true
		part, ok := byPart[partID]
		if !ok {
			added = append(added, c.convertAttachment(ctx, portal, intent, att))
			continue
		}
		meta, _ := part.Metadata.(*chattoid.MessageMetadata)
		if meta == nil || !meta.Processing {
			continue
		}
		converted := c.convertAttachment(ctx, portal, intent, att)
		if newMeta, _ := converted.DBMetadata.(*chattoid.MessageMetadata); newMeta != nil && newMeta.Processing {
			continue
		}
		edit.ModifiedParts = append(edit.ModifiedParts, converted.ToEditPart(part))
	}
	for _, part := range existing {
		if _, isAttachment := chattoid.ParseAttachmentPartID(part.PartID); isAttachment && !seen[part.PartID] {
			edit.DeletedParts = append(edit.DeletedParts, part)
		}
	}
	if len(added) > 0 {
		edit.AddedParts = &bridgev2.ConvertedMessage{Parts: added}
	}
	slices.SortFunc(edit.ModifiedParts, func(a, b *bridgev2.ConvertedEditPart) int {
		return strings.Compare(string(a.Part.PartID), string(b.Part.PartID))
	})
	return edit
}
