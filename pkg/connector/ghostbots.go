package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	adminv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/admin/v1"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
)

// ghostBot is a Chatto bot account that stands in for one Matrix user who has
// no Chatto login (ADR 0005).
type ghostBot struct {
	UserID string `json:"user_id"`
	Login  string `json:"login"`
	APIKey string `json:"api_key"`
	Owner  string `json:"owner"`
	// Rooms the bot was already added to and granted permissions in.
	Rooms []string `json:"rooms,omitempty"`
	// DisplayName is the name the bot was last given.
	DisplayName string `json:"display_name,omitempty"`
}

func (b *ghostBot) inRoom(roomID string) bool {
	for _, r := range b.Rooms {
		if r == roomID {
			return true
		}
	}
	return false
}

// ghostBots stores and creates ghost bots. It is shared by all logins.
type ghostBots struct {
	mu     sync.Mutex
	byMXID map[id.UserID]*ghostBot
	byUser map[string]id.UserID // Chatto user ID → Matrix user
	loaded bool
}

func ghostBotKey(mxid id.UserID) database.Key {
	return database.Key("chatto_ghost_bot:" + string(mxid))
}

const ghostBotIndexKey = database.Key("chatto_ghost_bot_index")

func (c *Connector) loadGhostBotsLocked(ctx context.Context) {
	gb := &c.ghostBots
	if gb.loaded {
		return
	}
	gb.byMXID = make(map[id.UserID]*ghostBot)
	gb.byUser = make(map[string]id.UserID)
	var index []id.UserID
	if raw := c.Bridge.DB.KV.Get(ctx, ghostBotIndexKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &index)
	}
	for _, mxid := range index {
		var bot ghostBot
		if raw := c.Bridge.DB.KV.Get(ctx, ghostBotKey(mxid)); raw != "" && json.Unmarshal([]byte(raw), &bot) == nil {
			gb.byMXID[mxid] = &bot
			gb.byUser[bot.UserID] = mxid
		}
	}
	gb.loaded = true
}

func (c *Connector) saveGhostBotLocked(ctx context.Context, mxid id.UserID, bot *ghostBot) {
	gb := &c.ghostBots
	gb.byMXID[mxid] = bot
	gb.byUser[bot.UserID] = mxid
	data, _ := json.Marshal(bot)
	c.Bridge.DB.KV.Set(ctx, ghostBotKey(mxid), string(data))
	index := make([]id.UserID, 0, len(gb.byMXID))
	for m := range gb.byMXID {
		index = append(index, m)
	}
	indexData, _ := json.Marshal(index)
	c.Bridge.DB.KV.Set(ctx, ghostBotIndexKey, string(indexData))
}

// IsGhostBot reports whether a Chatto user is a ghost bot made by the bridge.
// Such users represent Matrix users and must not get ghosts of their own.
func (c *Connector) IsGhostBot(ctx context.Context, chattoUserID string) bool {
	if !c.Config.GhostBots.Enabled {
		return false
	}
	c.ghostBots.mu.Lock()
	defer c.ghostBots.mu.Unlock()
	c.loadGhostBotsLocked(ctx)
	_, ok := c.ghostBots.byUser[chattoUserID]
	return ok
}

// ghostBotLogin derives a valid, stable Chatto login for a Matrix user.
func ghostBotLogin(prefix string, mxid id.UserID) string {
	localpart, _, _ := mxid.Parse()
	var sb strings.Builder
	for _, r := range strings.ToLower(localpart) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			sb.WriteRune(r)
		}
	}
	clean := strings.Trim(sb.String(), ".")
	sum := sha256.Sum256([]byte(mxid))
	suffix := "-" + hex.EncodeToString(sum[:])[:4]
	maxLocal := 32 - len(prefix) - len(suffix)
	if len(clean) > maxLocal {
		clean = clean[:maxLocal]
	}
	login := prefix + strings.TrimRight(clean, ".") + suffix
	if first := login[0]; (first < 'a' || first > 'z') && (first < '0' || first > '9') {
		login = "m" + login
	}
	if len(login) > 32 {
		login = login[:32]
	}
	return login
}

func origSenderName(sender *bridgev2.OrigSender) string {
	switch {
	case sender.DisambiguatedName != "":
		return sender.DisambiguatedName
	case sender.Displayname != "":
		return sender.Displayname
	default:
		localpart, _, _ := sender.UserID.Parse()
		return localpart
	}
}

var errGhostBotsUnavailable = errors.New("ghost bots need a human relay login")

// ensureGhostBot returns the ghost bot for a relayed Matrix user, creating it
// and adding it to the room on first use. owner is the relay login.
func (c *ChattoClient) ensureGhostBot(ctx context.Context, sender *bridgev2.OrigSender, roomID string) (*ghostBot, error) {
	if c.meta().IsBot {
		return nil, errGhostBotsUnavailable
	}
	main := c.Main
	main.ghostBots.mu.Lock()
	defer main.ghostBots.mu.Unlock()
	main.loadGhostBotsLocked(ctx)
	bot := main.ghostBots.byMXID[sender.UserID]
	name := main.Config.FormatGhostBotDisplayname(GhostBotParams{DisplayName: origSenderName(sender), UserID: string(sender.UserID)})
	if name == "" {
		name = string(sender.UserID)
	}
	if bot == nil {
		login := ghostBotLogin(main.Config.GhostBots.LoginPrefix, sender.UserID)
		keyName := "mautrix-chatto"
		resp, err := c.api.Bots.CreateBot(ctx, connect.NewRequest(&apiv1.CreateBotRequest{
			Login: login, DisplayName: name, ApiKeyName: &keyName,
		}))
		if err != nil {
			return nil, fmt.Errorf("failed to create ghost bot: %w", err)
		}
		bot = &ghostBot{
			UserID: resp.Msg.GetBot().GetUser().GetId(), Login: login,
			APIKey: resp.Msg.GetApiKey(), Owner: c.userID, DisplayName: name,
		}
		main.saveGhostBotLocked(ctx, sender.UserID, bot)
		zerolog.Ctx(ctx).Info().Str("bot_id", bot.UserID).Str("bot_login", login).
			Stringer("matrix_user", sender.UserID).Msg("Created ghost bot")
	} else if bot.DisplayName != name {
		nameCopy := name
		_, err := c.api.Users.UpdateUserProfile(ctx, connect.NewRequest(&apiv1.UpdateUserProfileRequest{
			UserId: bot.UserID, DisplayName: &nameCopy,
		}))
		if err == nil {
			bot.DisplayName = name
			main.saveGhostBotLocked(ctx, sender.UserID, bot)
		}
	}
	if !bot.inRoom(roomID) {
		for _, perm := range BotPermissions {
			_, err := c.api.AdminPermissions.SetUserPermission(ctx, connect.NewRequest(&adminv1.SetUserPermissionRequest{
				UserId:     bot.UserID,
				Permission: perm,
				Decision:   adminv1.PermissionDecision_PERMISSION_DECISION_ALLOW,
				Scope:      &adminv1.PermissionScope{Kind: adminv1.PermissionScopeKind_PERMISSION_SCOPE_KIND_ROOM, Id: roomID},
			}))
			if err != nil {
				return nil, fmt.Errorf("failed to grant %s to ghost bot: %w", perm, err)
			}
		}
		_, err := c.api.Rooms.AddMember(ctx, connect.NewRequest(&apiv1.AddMemberRequest{RoomId: roomID, UserId: bot.UserID}))
		if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
			return nil, fmt.Errorf("failed to add ghost bot to room: %w", err)
		}
		bot.Rooms = append(bot.Rooms, roomID)
		main.saveGhostBotLocked(ctx, sender.UserID, bot)
	}
	return bot, nil
}

// ghostBotClient returns an API client authenticated as a ghost bot.
func (c *ChattoClient) ghostBotClient(bot *ghostBot) *chattoclient.Client {
	return c.api.WithTokens(chattoclient.StaticToken(bot.APIKey))
}

// ghostBotForMessage returns the ghost bot that sent a bridged message, if
// the message was sent through one.
func (c *Connector) ghostBotForMessage(ctx context.Context, senderID string) *ghostBot {
	if !c.Config.GhostBots.Enabled {
		return nil
	}
	c.ghostBots.mu.Lock()
	defer c.ghostBots.mu.Unlock()
	c.loadGhostBotsLocked(ctx)
	if mxid, ok := c.ghostBots.byUser[senderID]; ok {
		return c.ghostBots.byMXID[mxid]
	}
	return nil
}
