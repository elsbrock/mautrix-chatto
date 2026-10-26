// Package chattoid maps Chatto identifiers to bridgev2 network IDs.
//
// Chatto IDs are server-scoped NanoIDs with a type prefix (R… rooms, E…
// events, U… users). One bridge serves several servers, so every network ID
// is the configured server ID, a dot, and the Chatto ID. The dot keeps ghost
// MXID localparts readable, and neither server IDs nor Chatto IDs contain
// one. Room groups and the server itself share the portal ID namespace with
// rooms and are marked to keep them apart.
package chattoid

import (
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

const (
	sep          = "."
	groupPrefix  = "group:"
	serverPortal = "server"
	noticePrefix = "notice:"
	// AttachmentPartPrefix prefixes message part IDs of attachments.
	AttachmentPartPrefix = "att:"
)

// PortalKind says what a portal ID refers to.
type PortalKind int

const (
	// PortalInvalid is a portal ID that does not belong to this bridge.
	PortalInvalid PortalKind = iota
	PortalRoom
	PortalGroup
	PortalServer
)

func join(server, id string) string {
	return server + sep + id
}

// split returns the server and Chatto parts of a network ID. Both are empty
// if the ID has no server prefix.
func split(id string) (server, rest string) {
	server, rest, ok := strings.Cut(id, sep)
	if !ok || server == "" || rest == "" {
		return "", ""
	}
	return server, rest
}

// MakeUserID returns the ghost ID of a Chatto user.
func MakeUserID(server, chattoUserID string) networkid.UserID {
	return networkid.UserID(join(server, chattoUserID))
}

// ParseUserID returns the server and Chatto user ID of a ghost.
func ParseUserID(id networkid.UserID) (server, chattoUserID string) {
	return split(string(id))
}

// MakeUserLoginID returns the login ID for a Chatto account.
func MakeUserLoginID(server, chattoUserID string) networkid.UserLoginID {
	return networkid.UserLoginID(join(server, chattoUserID))
}

// ParseUserLoginID returns the server and Chatto user ID of a login.
func ParseUserLoginID(id networkid.UserLoginID) (server, chattoUserID string) {
	return split(string(id))
}

// MakePortalID returns the portal ID of a Chatto room.
func MakePortalID(server, roomID string) networkid.PortalID {
	return networkid.PortalID(join(server, roomID))
}

// MakePortalKey returns the portal key of a Chatto room. Room IDs are global
// on a server and DMs have one room for all participants, so portals are
// never split per receiver.
func MakePortalKey(server, roomID string) networkid.PortalKey {
	return networkid.PortalKey{ID: MakePortalID(server, roomID)}
}

// MakeGroupPortalID returns the portal (space) ID of a Chatto room group.
func MakeGroupPortalID(server, groupID string) networkid.PortalID {
	return networkid.PortalID(join(server, groupPrefix+groupID))
}

// MakeServerPortalID returns the portal (space) ID of a Chatto server.
func MakeServerPortalID(server string) networkid.PortalID {
	return networkid.PortalID(join(server, serverPortal))
}

// ParsePortalID returns the server, kind and Chatto ID of a portal. The ID
// is empty for a server space.
func ParsePortalID(id networkid.PortalID) (server string, kind PortalKind, chattoID string) {
	server, rest := split(string(id))
	switch {
	case server == "":
		return "", PortalInvalid, ""
	case rest == serverPortal:
		return server, PortalServer, ""
	}
	if g, ok := strings.CutPrefix(rest, groupPrefix); ok {
		if g == "" {
			return "", PortalInvalid, ""
		}
		return server, PortalGroup, g
	}
	return server, PortalRoom, rest
}

// ParseRoomPortalID returns the server and room ID of a room portal, or
// empty strings for spaces and foreign IDs.
func ParseRoomPortalID(id networkid.PortalID) (server, roomID string) {
	server, kind, roomID := ParsePortalID(id)
	if kind != PortalRoom {
		return "", ""
	}
	return server, roomID
}

// MakeMessageID returns the message ID of a Chatto event.
func MakeMessageID(server, eventID string) networkid.MessageID {
	return networkid.MessageID(join(server, eventID))
}

// ParseMessageID returns the server and Chatto event ID of a message. Both
// are empty for bridge notices.
func ParseMessageID(id networkid.MessageID) (server, eventID string) {
	if IsNoticeMessageID(id) {
		return "", ""
	}
	return split(string(id))
}

// MakeNoticeMessageID returns the ID of a bridge notice, which has no
// Chatto counterpart.
func MakeNoticeMessageID(server, roomID, unique string) networkid.MessageID {
	return networkid.MessageID(noticePrefix + join(server, roomID) + ":" + unique)
}

// IsNoticeMessageID reports whether a message is a bridge notice.
func IsNoticeMessageID(id networkid.MessageID) bool {
	return strings.HasPrefix(string(id), noticePrefix)
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
