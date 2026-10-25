package connector

import (
	"context"
	"errors"
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

	userID string
	echoes *echoTracker
	users  *userCache

	stopLock sync.Mutex
	stop     context.CancelFunc
	running  sync.WaitGroup

	typingLock sync.Mutex
	typing     map[networkid.PortalID]context.CancelFunc

	cursorLock     sync.Mutex
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
	return &ChattoClient{
		Main:      main,
		UserLogin: login,
		userID:    chattoid.ParseUserLoginID(login.ID),
		echoes:    newEchoTracker(),
		typing:    make(map[networkid.PortalID]context.CancelFunc),
	}
}

func (c *ChattoClient) meta() *chattoid.UserLoginMetadata {
	return c.UserLogin.Metadata.(*chattoid.UserLoginMetadata)
}

// API returns the authenticated Chatto client of this login.
func (c *ChattoClient) API() *chattoclient.Client { return c.api }

func (c *ChattoClient) GetUserID() networkid.UserID {
	return chattoid.MakeUserID(c.userID)
}

func (c *ChattoClient) IsThisUser(_ context.Context, userID networkid.UserID) bool {
	return chattoid.ParseUserID(userID) == c.userID
}

func (c *ChattoClient) IsLoggedIn() bool {
	return c.api != nil
}

// persistSession stores rotated session tokens. It is called with the
// pending refresh ID before a refresh and with the new tokens after.
func (c *ChattoClient) persistSession(ctx context.Context, sess chattoclient.Session) error {
	meta := c.meta()
	meta.Session = &sess
	return c.UserLogin.Save(ctx)
}

func (c *ChattoClient) Connect(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	if c.api == nil {
		c.UserLogin.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "chatto-no-credentials",
			Message:    "No Chatto credentials stored, please log in again",
		})
		return
	}
	c.UserLogin.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
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
		if errors.Is(err, chattoclient.ErrRealtimeFatal) {
			c.badCredentials(runCtx, err)
		}
	}()
	if c.Main.Config.KeepLoginsOnline && !c.meta().IsBot {
		c.running.Add(1)
		go func() {
			defer c.running.Done()
			c.keepPresenceAlive(runCtx)
		}()
	}
}

func (c *ChattoClient) badCredentials(ctx context.Context, err error) {
	zerolog.Ctx(ctx).Warn().Err(err).Msg("Chatto rejected the login credentials")
	c.UserLogin.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateBadCredentials,
		Error:      "chatto-invalid-auth",
		Message:    "Chatto signed this login out, please log in again",
	})
}

func (c *ChattoClient) updateRemoteProfile(ctx context.Context, user *apiv1.User) {
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
	meta := c.meta()
	if meta.Kind == chattoid.CredentialSession && c.tokens != nil {
		if src, ok := c.tokens.(*chattoclient.SessionTokenSource); ok {
			if err := c.api.Logout(ctx, src.Session()); err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to revoke Chatto session")
			}
		}
	}
	meta.Session = nil
	meta.BotKey = ""
	meta.RealtimeCursor = ""
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
