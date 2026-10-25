// Package chattoid maps Chatto identifiers to bridgev2 network IDs.
//
// Chatto IDs are server-scoped NanoIDs with a type prefix (R… rooms, E…
// events, U… users), so they are used verbatim. Room groups share the portal
// ID namespace with rooms and are prefixed to keep them apart.
package chattoid

import (
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

const (
	groupPrefix = "group:"
	// AttachmentPartPrefix prefixes message part IDs of attachments.
	AttachmentPartPrefix = "att:"
)

// MakeUserID returns the ghost ID of a Chatto user.
func MakeUserID(chattoUserID string) networkid.UserID {
	return networkid.UserID(chattoUserID)
}

// ParseUserID returns the Chatto user ID of a ghost.
func ParseUserID(id networkid.UserID) string {
	return string(id)
}

// MakeUserLoginID returns the login ID for a Chatto account.
func MakeUserLoginID(chattoUserID string) networkid.UserLoginID {
	return networkid.UserLoginID(chattoUserID)
}

// ParseUserLoginID returns the Chatto user ID of a login.
func ParseUserLoginID(id networkid.UserLoginID) string {
	return string(id)
}

// MakePortalID returns the portal ID of a Chatto room.
func MakePortalID(roomID string) networkid.PortalID {
	return networkid.PortalID(roomID)
}

// MakePortalKey returns the portal key of a Chatto room. Room IDs are global
// on a server and DMs have one room for all participants, so portals are
// never split per receiver.
func MakePortalKey(roomID string) networkid.PortalKey {
	return networkid.PortalKey{ID: MakePortalID(roomID)}
}

// MakeGroupPortalID returns the portal (space) ID of a Chatto room group.
func MakeGroupPortalID(groupID string) networkid.PortalID {
	return networkid.PortalID(groupPrefix + groupID)
}

// ParsePortalID splits a portal ID into a room ID or a room group ID.
func ParsePortalID(id networkid.PortalID) (roomID, groupID string) {
	if g, ok := strings.CutPrefix(string(id), groupPrefix); ok {
		return "", g
	}
	return string(id), ""
}

// MakeMessageID returns the message ID of a Chatto event.
func MakeMessageID(eventID string) networkid.MessageID {
	return networkid.MessageID(eventID)
}

// ParseMessageID returns the Chatto event ID of a message.
func ParseMessageID(id networkid.MessageID) string {
	return string(id)
}

// MakeAttachmentPartID returns the part ID for one attachment of a message.
func MakeAttachmentPartID(attachmentID string) networkid.PartID {
	return networkid.PartID(AttachmentPartPrefix + attachmentID)
}

// ParseAttachmentPartID returns the attachment ID of a part, if it is one.
func ParseAttachmentPartID(id networkid.PartID) (string, bool) {
	return strings.CutPrefix(string(id), AttachmentPartPrefix)
}

// MakeEmojiID returns the emoji ID for a Chatto reaction shortcode.
func MakeEmojiID(shortcode string) networkid.EmojiID {
	return networkid.EmojiID(shortcode)
}
