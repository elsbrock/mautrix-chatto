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
	"time"
	"unicode"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
	adminv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/admin/v1"
	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
)

// ghostBot is a Chatto bot account that stands in for one Matrix user who has
// no Chatto login (ADR 0005).
type ghostBot struct {
	UserID string `json:"user_id"`
	Login  string `json:"login"`
	APIKey string `json:"api_key"`
	Owner  string `json:"owner"`
	// Server is the configured ID of the bot's Chatto server.
	Server string `json:"server"`
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

// ghostBots stores and creates ghost bots. It is shared by all logins. mu
// only guards the maps; creating or updating one user's bot is serialised by
// that user's lock so that RPCs never run under mu.
type ghostBots struct {
	mu        sync.Mutex
	byKey     map[ghostBotID]*ghostBot
	byUser    map[networkid.UserID]ghostBotID // bot's ghost ID → key
	userLocks map[ghostBotID]*sync.Mutex
	failures  map[ghostBotID]time.Time
	loaded    bool
}

// ghostBotID identifies a ghost bot: one per Matrix user and relay login,
// because only a bot's owner can grant it permissions in rooms.
type ghostBotID string

func makeGhostBotID(owner networkid.UserLoginID, mxid id.UserID) ghostBotID {
	return ghostBotID(string(owner) + "|" + string(mxid))
}

// ghostBotRetryAfter is how long creation is not retried after a failure.
const ghostBotRetryAfter = 10 * time.Minute

// lockUser serialises ghost bot work for one Matrix user.
func (gb *ghostBots) lockUser(key ghostBotID) func() {
	gb.mu.Lock()
	if gb.userLocks == nil {
		gb.userLocks = make(map[ghostBotID]*sync.Mutex)
	}
	l, ok := gb.userLocks[key]
	if !ok {
		l = &sync.Mutex{}
		gb.userLocks[key] = l
	}
	gb.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// get returns a copy of a user's ghost bot, if any.
func (c *Connector) getGhostBot(ctx context.Context, key ghostBotID) *ghostBot {
	c.ghostBots.mu.Lock()
	defer c.ghostBots.mu.Unlock()
	c.loadGhostBotsLocked(ctx)
	if bot, ok := c.ghostBots.byKey[key]; ok {
		cp := *bot
		cp.Rooms = append([]string(nil), bot.Rooms...)
		return &cp
	}
	return nil
}

func (c *Connector) storeGhostBot(ctx context.Context, key ghostBotID, bot *ghostBot) {
	c.ghostBots.mu.Lock()
	defer c.ghostBots.mu.Unlock()
	c.loadGhostBotsLocked(ctx)
	c.saveGhostBotLocked(ctx, key, bot)
}

func ghostBotKVKey(key ghostBotID) database.Key {
	return database.Key("chatto_ghost_bot:" + string(key))
}

const ghostBotIndexKey = database.Key("chatto_ghost_bot_index_v2")

func (c *Connector) loadGhostBotsLocked(ctx context.Context) {
	gb := &c.ghostBots
	if gb.loaded {
		return
	}
	gb.byKey = make(map[ghostBotID]*ghostBot)
	gb.byUser = make(map[networkid.UserID]ghostBotID)
	gb.failures = make(map[ghostBotID]time.Time)
	var index []ghostBotID
	if raw := c.Bridge.DB.KV.Get(ctx, ghostBotIndexKey); raw != "" {
		_ = json.Unmarshal([]byte(raw), &index)
	}
	for _, key := range index {
		var bot ghostBot
		if raw := c.Bridge.DB.KV.Get(ctx, ghostBotKVKey(key)); raw != "" && json.Unmarshal([]byte(raw), &bot) == nil {
			gb.byKey[key] = &bot
			gb.byUser[bot.ghostID()] = key
		}
	}
	gb.loaded = true
}

func (c *Connector) saveGhostBotLocked(ctx context.Context, key ghostBotID, bot *ghostBot) {
	gb := &c.ghostBots
	stored := *bot
	stored.Rooms = append([]string(nil), bot.Rooms...)
	gb.byKey[key] = &stored
	gb.byUser[bot.ghostID()] = key
	data, _ := json.Marshal(&stored)
	c.Bridge.DB.KV.Set(ctx, ghostBotKVKey(key), string(data))
	index := make([]ghostBotID, 0, len(gb.byKey))
	for k := range gb.byKey {
		index = append(index, k)
	}
	indexData, _ := json.Marshal(index)
	c.Bridge.DB.KV.Set(ctx, ghostBotIndexKey, string(indexData))
}

// ghostID returns the ghost ID the bot's Chatto account would have.
func (b *ghostBot) ghostID() networkid.UserID {
	return chattoid.MakeUserID(b.Server, b.UserID)
}

// IsGhostBot reports whether a Chatto user is a ghost bot made by the bridge.
// Such users represent Matrix users and must not get ghosts of their own.
func (c *Connector) IsGhostBot(ctx context.Context, ghostID networkid.UserID) bool {
	if !c.Config.GhostBots.Enabled {
		return false
	}
	c.ghostBots.mu.Lock()
	defer c.ghostBots.mu.Unlock()
	c.loadGhostBotsLocked(ctx)
	_, ok := c.ghostBots.byUser[ghostID]
	return ok
}

// ghostBotLogin derives a valid, stable Chatto login for a Matrix user. The
// suffix hashes the user and salt (the owner); hashLen grows on collisions.
func ghostBotLogin(prefix string, mxid id.UserID, salt string, hashLen int) string {
	localpart, _, _ := mxid.Parse()
	var sb strings.Builder
	for _, r := range strings.ToLower(localpart) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			sb.WriteRune(r)
		}
	}
	clean := strings.Trim(sb.String(), ".")
	sum := sha256.Sum256([]byte(salt + "|" + string(mxid)))
	suffix := "-" + hex.EncodeToString(sum[:])[:hashLen]
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
	if c.isBot() {
		return nil, errGhostBotsUnavailable
	}
	main := c.Main
	key := makeGhostBotID(c.UserLogin.ID, sender.UserID)
	unlock := main.ghostBots.lockUser(key)
	defer unlock()
	bot := main.getGhostBot(ctx, key)
	name := sanitizeDisplayName(main.Config.FormatGhostBotDisplayname(GhostBotParams{DisplayName: origSenderName(sender), UserID: string(sender.UserID)}))
	if name == "" {
		name = truncateRunes(string(sender.UserID), 32)
	}
	if bot == nil {
		if main.ghostBotFailedRecently(key) {
			return nil, errGhostBotsUnavailable
		}
		var err error
		bot, err = c.createGhostBot(ctx, sender.UserID, name)
		if err != nil {
			main.recordGhostBotFailure(key)
			return nil, err
		}
		main.storeGhostBot(ctx, key, bot)
		zerolog.Ctx(ctx).Info().Str("bot_id", bot.UserID).Str("bot_login", bot.Login).
			Stringer("matrix_user", sender.UserID).Msg("Created ghost bot")
	} else if bot.DisplayName != name {
		nameCopy := name
		_, err := c.api.Users.UpdateUserProfile(ctx, connect.NewRequest(&apiv1.UpdateUserProfileRequest{
			UserId: bot.UserID, DisplayName: &nameCopy,
		}))
		if err == nil {
			bot.DisplayName = name
			main.storeGhostBot(ctx, key, bot)
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
		main.storeGhostBot(ctx, key, bot)
	}
	return bot, nil
}

// ghostBotClient returns an API client authenticated as a ghost bot.
func (c *ChattoClient) ghostBotClient(bot *ghostBot) *chattoclient.Client {
	return c.api.WithTokens(chattoclient.StaticToken(bot.APIKey))
}

// ghostBotForMessage returns the ghost bot that sent a bridged message, if
// the message was sent through one.
func (c *Connector) ghostBotForMessage(ctx context.Context, senderID networkid.UserID) *ghostBot {
	if !c.Config.GhostBots.Enabled {
		return nil
	}
	c.ghostBots.mu.Lock()
	defer c.ghostBots.mu.Unlock()
	c.loadGhostBotsLocked(ctx)
	if key, ok := c.ghostBots.byUser[senderID]; ok {
		return c.ghostBots.byKey[key]
	}
	return nil
}

// createGhostBot creates the Chatto bot, trying longer login suffixes if a
// login is already taken.
func (c *ChattoClient) createGhostBot(ctx context.Context, mxid id.UserID, name string) (*ghostBot, error) {
	keyName := "mautrix-chatto"
	var lastErr error
	for _, hashLen := range []int{4, 8, 12} {
		login := ghostBotLogin(c.Main.Config.GhostBots.LoginPrefix, mxid, c.userID, hashLen)
		resp, err := c.api.Bots.CreateBot(ctx, connect.NewRequest(&apiv1.CreateBotRequest{
			Login: login, DisplayName: name, ApiKeyName: &keyName,
		}))
		if connect.CodeOf(err) == connect.CodeAlreadyExists {
			lastErr = err
			continue
		} else if err != nil {
			return nil, fmt.Errorf("failed to create ghost bot: %w", err)
		}
		return &ghostBot{
			UserID: resp.Msg.GetBot().GetUser().GetId(), Login: login,
			APIKey: resp.Msg.GetApiKey(), Owner: c.userID, Server: c.serverID, DisplayName: name,
		}, nil
	}
	return nil, fmt.Errorf("failed to create ghost bot, all logins taken: %w", lastErr)
}

func (c *Connector) ghostBotFailedRecently(key ghostBotID) bool {
	c.ghostBots.mu.Lock()
	defer c.ghostBots.mu.Unlock()
	at, ok := c.ghostBots.failures[key]
	return ok && time.Since(at) < ghostBotRetryAfter
}

func (c *Connector) recordGhostBotFailure(key ghostBotID) {
	c.ghostBots.mu.Lock()
	defer c.ghostBots.mu.Unlock()
	if c.ghostBots.failures == nil {
		c.ghostBots.failures = make(map[ghostBotID]time.Time)
	}
	c.ghostBots.failures[key] = time.Now()
}

// sanitizeDisplayName drops control and format characters, which Chatto
// rejects in display names, except the joiners emoji sequences need.
func sanitizeDisplayName(name string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == '\u200C' || r == '\u200D' || (r >= '\U000E0020' && r <= '\U000E007F') {
			return r
		}
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name))
}
