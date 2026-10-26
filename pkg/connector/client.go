package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
)

// ChattoClient is the bridgev2 NetworkAPI for one Chatto login.
type ChattoClient struct {
	Main      *Connector
	UserLogin *bridgev2.UserLogin

	api    *chattoclient.Client
	tokens chattoclient.TokenSource
	rt     *chattoclient.Realtime

	// serverID is the server part of the login ID. srv is nil when that
	// server is no longer configured.
	serverID string
	srv      *server
	userID   string
	users    *userCache

	stopLock sync.Mutex
	stop     context.CancelFunc
	running  sync.WaitGroup

	typingLock sync.Mutex
	typing     map[networkid.PortalID]context.CancelFunc

	// metaLock guards UserLogin.Metadata and serialises saves, which happen
	// from the realtime loop, token refreshes and profile updates.
	metaLock       sync.Mutex
	lastCursorSave time.Time
}

var (
	_ bridgev2.NetworkAPI                    = (*ChattoClient)(nil)
	_ bridgev2.NetworkAPIWithUserID          = (*ChattoClient)(nil)
	_ bridgev2.EditHandlingNetworkAPI        = (*ChattoClient)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI    = (*ChattoClient)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI   = (*ChattoClient)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI = (*ChattoClient)(nil)
	_ bridgev2.TypingHandlingNetworkAPI      = (*ChattoClient)(nil)
	_ bridgev2.RoomNameHandlingNetworkAPI    = (*ChattoClient)(nil)
	_ bridgev2.RoomTopicHandlingNetworkAPI   = (*ChattoClient)(nil)
	_ bridgev2.MembershipHandlingNetworkAPI  = (*ChattoClient)(nil)
	_ bridgev2.BackfillingNetworkAPI         = (*ChattoClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*ChattoClient)(nil)
	_ bridgev2.GhostDMCreatingNetworkAPI     = (*ChattoClient)(nil)
	_ bridgev2.ContactListingNetworkAPI      = (*ChattoClient)(nil)
	_ bridgev2.UserSearchingNetworkAPI       = (*ChattoClient)(nil)
)

func newChattoClient(main *Connector, login *bridgev2.UserLogin) *ChattoClient {
	serverID, userID := chattoid.ParseUserLoginID(login.ID)
	return &ChattoClient{
		Main:      main,
		UserLogin: login,
		serverID:  serverID,
		srv:       main.server(serverID),
		userID:    userID,
		typing:    make(map[networkid.PortalID]context.CancelFunc),
	}
}

// ghostID returns the ghost ID of a user on this login's server.
func (c *ChattoClient) ghostID(chattoUserID string) networkid.UserID {
	return chattoid.MakeUserID(c.serverID, chattoUserID)
}

// portalKey returns the portal key of a room on this login's server.
func (c *ChattoClient) portalKey(roomID string) networkid.PortalKey {
	return chattoid.MakePortalKey(c.serverID, roomID)
}

// messageID returns the message ID of an event on this login's server.
func (c *ChattoClient) messageID(eventID string) networkid.MessageID {
	return chattoid.MakeMessageID(c.serverID, eventID)
}

// chattoUserID returns the Chatto user ID of a ghost, if it is on this
// login's server.
func (c *ChattoClient) chattoUserID(id networkid.UserID) (string, bool) {
	srv, userID := chattoid.ParseUserID(id)
	return userID, srv == c.serverID && userID != ""
}

// chattoEventID returns the Chatto event ID of a message, if it is on this
// login's server.
func (c *ChattoClient) chattoEventID(id networkid.MessageID) (string, bool) {
	srv, eventID := chattoid.ParseMessageID(id)
	return eventID, srv == c.serverID && eventID != ""
}

func (c *ChattoClient) meta() *chattoid.UserLoginMetadata {
	return c.UserLogin.Metadata.(*chattoid.UserLoginMetadata)
}

// API returns the authenticated Chatto client of this login.
func (c *ChattoClient) API() *chattoclient.Client { return c.api }

func (c *ChattoClient) GetUserID() networkid.UserID {
	return c.ghostID(c.userID)
}

func (c *ChattoClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	id, ok := c.chattoUserID(userID)
	return ok && id == c.userID
}

func (c *ChattoClient) IsLoggedIn() bool {
	return c.api != nil
}

// persistSession stores rotated session tokens. It is called with the
// pending refresh ID before a refresh and with the new tokens after.
func (c *ChattoClient) persistSession(ctx context.Context, sess chattoclient.Session) error {
	c.metaLock.Lock()
	defer c.metaLock.Unlock()
	c.meta().Session = &sess
	return c.UserLogin.Save(ctx)
}

// isBot reports whether this login is a Chatto bot account.
func (c *ChattoClient) isBot() bool {
	c.metaLock.Lock()
	defer c.metaLock.Unlock()
	return c.meta().IsBot
}

func (c *ChattoClient) Connect(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	if c.srv == nil {
		c.sendState(status.BridgeState{
			StateEvent: status.StateUnknownError,
			Error:      "chatto-server-not-configured",
			Message:    fmt.Sprintf("The Chatto server %q is no longer configured on this bridge; log out or ask the bridge admin to add it back", c.serverID),
		})
		return
	}
	if version, _ := c.srv.serverVersion(); version == "" {
		// The server was unreachable when the bridge started.
		if err := c.Main.checkServerVersion(ctx, c.srv); err != nil {
			log.Warn().Err(err).Msg("Failed to fetch Chatto server version")
		}
	}
	if _, tooOld := c.srv.serverVersion(); tooOld {
		c.sendState(status.BridgeState{
			StateEvent: status.StateUnknownError,
			Error:      "chatto-server-too-old",
			Message:    fmt.Sprintf("The Chatto server %q is older than %s, which this bridge requires", c.serverID, chattoclient.UpstreamVersion),
		})
		return
	}
	if c.api == nil {
		c.sendState(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "chatto-no-credentials",
			Message:    "No Chatto credentials stored, please log in again",
		})
		return
	}
	c.sendState(status.BridgeState{StateEvent: status.StateConnecting})
	if c.users == nil {
		c.users = newUserCache(c.api)
	}
	viewer, err := c.api.Viewer.GetViewer(ctx, connect.NewRequest(&apiv1.GetViewerRequest{}))
	if err != nil {
		if isAuthError(err) {
			c.badCredentials(ctx, err)
			return
		}
		log.Err(err).Msg("Failed to fetch Chatto viewer, will retry via realtime")
	} else if profile := viewer.Msg.GetUser().GetProfile(); profile != nil {
		c.updateRemoteProfile(ctx, profile)
	}
	c.ensureServerSpace(ctx)

	runCtx, cancel := context.WithCancel(c.UserLogin.Log.WithContext(c.Main.Bridge.BackgroundCtx))
	c.stopLock.Lock()
	if c.stop != nil {
		c.stop()
	}
	c.stop = cancel
	c.stopLock.Unlock()

	c.rt = chattoclient.NewRealtime(c.api, c, c.meta().RealtimeCursor, string(c.UserLogin.ID), c.UserLogin.Log.With().Str("component", "realtime").Logger())
	c.running.Add(1)
	go func() {
		defer c.running.Done()
		err := c.rt.Run(runCtx)
		switch {
		case !errors.Is(err, chattoclient.ErrRealtimeFatal):
		case chattoclient.IsCredentialError(err):
			c.badCredentials(runCtx, err)
		default:
			// Protocol mismatch or a rejected subscription: logging in
			// again will not help, the bridge or server needs updating.
			c.sendState(status.BridgeState{
				StateEvent: status.StateUnknownError,
				Error:      "chatto-realtime-rejected",
				Message:    fmt.Sprintf("Chatto rejected the realtime connection: %v", err),
			})
		}
	}()
	if c.Main.Config.KeepLoginsOnline && !c.isBot() {
		c.running.Add(1)
		go func() {
			defer c.running.Done()
			c.keepPresenceAlive(runCtx)
		}()
	}
}

func (c *ChattoClient) badCredentials(ctx context.Context, err error) {
	zerolog.Ctx(ctx).Warn().Err(err).Msg("Chatto rejected the login credentials")
	c.sendState(status.BridgeState{
		StateEvent: status.StateBadCredentials,
		Error:      "chatto-invalid-auth",
		Message:    "Chatto signed this login out, please log in again",
	})
}

func (c *ChattoClient) updateRemoteProfile(ctx context.Context, user *apiv1.User) {
	c.metaLock.Lock()
	defer c.metaLock.Unlock()
	changed := false
	if c.UserLogin.RemoteProfile.Username != user.GetLogin() {
		c.UserLogin.RemoteProfile.Username = user.GetLogin()
		changed = true
	}
	if c.UserLogin.RemoteProfile.Name != user.GetDisplayName() {
		c.UserLogin.RemoteProfile.Name = user.GetDisplayName()
		changed = true
	}
	if meta := c.meta(); meta.Login != user.GetLogin() || meta.IsBot != (user.GetBot() != nil) {
		meta.Login = user.GetLogin()
		meta.IsBot = user.GetBot() != nil
		changed = true
	}
	if changed {
		if err := c.UserLogin.Save(ctx); err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to save remote profile")
		}
	}
}

func (c *ChattoClient) Disconnect() {
	c.stopLock.Lock()
	stop := c.stop
	c.stop = nil
	c.stopLock.Unlock()
	if stop != nil {
		stop()
	}
	c.running.Wait()
	c.typingLock.Lock()
	for _, cancel := range c.typing {
		cancel()
	}
	clear(c.typing)
	c.typingLock.Unlock()
}

func (c *ChattoClient) LogoutRemote(ctx context.Context) {
	c.Disconnect()
	// Revoke before taking metaLock: a token refresh holds the token
	// source's lock and then takes metaLock to persist.
	if src, ok := c.tokens.(*chattoclient.SessionTokenSource); ok && c.api != nil {
		if err := c.api.Logout(ctx, src.Session()); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to revoke Chatto session")
		}
	}
	c.metaLock.Lock()
	defer c.metaLock.Unlock()
	meta := c.meta()
	meta.Session = nil
	meta.BotKey = ""
	meta.RealtimeCursor = ""
	c.Main.recordLoginState(c.UserLogin.ID, "")
	c.api = nil
	c.tokens = nil
}

// keepPresenceAlive refreshes the login's Chatto presence every 30 seconds,
// which keeps it online for as long as the bridge runs.
func (c *ChattoClient) keepPresenceAlive(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if _, err := c.api.Account.RefreshPresence(ctx, connect.NewRequest(&apiv1.RefreshPresenceRequest{})); err != nil && ctx.Err() == nil {
			zerolog.Ctx(ctx).Debug().Err(err).Msg("Failed to refresh presence")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func isAuthError(err error) bool {
	if errors.Is(err, chattoclient.ErrSessionRevoked) {
		return true
	}
	code := connect.CodeOf(err)
	return code == connect.CodeUnauthenticated
}

// sendState reports a bridge state and records it in the login gauge.
func (c *ChattoClient) sendState(state status.BridgeState) {
	c.Main.recordLoginState(c.UserLogin.ID, state.StateEvent)
	c.UserLogin.BridgeState.Send(state)
}

// ensureServerSpace creates the server space before any room of the server,
// or refreshes its name, topic and avatar if it exists.
//
// bridgev2 creates a missing parent space from the child room's event
// context and abandons it when that context ends, which can leave the space
// half-created; creating it up front avoids that path.
func (c *ChattoClient) ensureServerSpace(ctx context.Context) {
	portal, err := c.Main.Bridge.GetPortalByKey(ctx, networkid.PortalKey{ID: chattoid.MakeServerPortalID(c.serverID)})
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to get server space portal")
		return
	}
	info := c.getServerInfo(ctx)
	if portal.MXID != "" {
		portal.UpdateInfo(ctx, info, c.UserLogin, nil, time.Time{})
		return
	}
	if err := portal.CreateMatrixRoom(ctx, c.UserLogin, info); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to create server space")
	}
}
