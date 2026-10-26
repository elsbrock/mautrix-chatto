package chattoid

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
)

// CredentialKind says how a login authenticates.
type CredentialKind string

const (
	CredentialSession CredentialKind = "session"
	CredentialBotKey  CredentialKind = "bot_key"
)

// UserLoginMetadata is stored with each login.
type UserLoginMetadata struct {
	Kind    CredentialKind        `json:"kind"`
	Session *chattoclient.Session `json:"session,omitempty"`
	BotKey  string                `json:"bot_key,omitempty"`
	Login   string                `json:"login"`
	IsBot   bool                  `json:"is_bot,omitempty"`
	// Server is the configured server ID, also part of the login ID.
	Server string `json:"server,omitempty"`
	// RealtimeCursor is the last safe realtime resume cursor. Cursors expire
	// after 15 minutes, so it only helps across quick restarts.
	RealtimeCursor string `json:"realtime_cursor,omitempty"`
}

// GhostMetadata is stored with each ghost.
type GhostMetadata struct {
	Login string `json:"login,omitempty"`
	IsBot bool   `json:"is_bot,omitempty"`
}

// PortalMetadata is stored with each portal.
type PortalMetadata struct {
	Kind          string `json:"kind,omitempty"` // channel, dm, group_dm, space, server
	ThreadingMode int32  `json:"threading_mode,omitempty"`
	Universal     bool   `json:"universal,omitempty"`
	Archived      bool   `json:"archived,omitempty"`
}

// MessageMetadata is stored with each bridged message part.
type MessageMetadata struct {
	// AssetID is set for attachment parts.
	AssetID string `json:"asset_id,omitempty"`
	// Processing is true while Chatto was still processing the asset when
	// the part was bridged, so a later success event triggers an edit.
	Processing bool `json:"processing,omitempty"`
	// TextHash identifies the Chatto body last bridged into a text part, so
	// that events which don't change the text don't produce Matrix edits.
	TextHash string `json:"text_hash,omitempty"`
}

// HashText returns the TextHash of a Chatto message body.
func HashText(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}
