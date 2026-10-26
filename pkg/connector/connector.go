// Package connector implements the bridgev2 network connector for Chatto.
package connector

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/elsbrock/mautrix-chatto/pkg/bridgemetrics"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
)

// Connector is the bridgev2 NetworkConnector for the configured Chatto servers.
type Connector struct {
	Bridge *bridgev2.Bridge
	Config Config

	// HTTPClient is used for all Chatto requests. Tests replace it.
	HTTPClient *http.Client
	// Version is the bridge version, used in the user agent.
	Version string

	serversLock   sync.Mutex
	servers       map[string]*server
	metricsServer *http.Server
	// echoes is shared by all logins: any login's realtime stream may see
	// a message another login (or a ghost bot) is still sending.
	echoes    *echoTracker
	ghostBots ghostBots
	oauth     oauthState
	// allowInsecureOAuth permits a plain-HTTP public address (tests only).
	allowInsecureOAuth bool

	readyLock sync.RWMutex
	ready     bool

	loginStatesLock sync.Mutex
	loginStates     map[networkid.UserLoginID]loginState
}

// loginState is a login's last bridge state and the server it belongs to.
type loginState struct {
	server string
	state  status.BridgeStateEvent
}

// recordLoginState updates the per-server, per-state login gauge.
func (c *Connector) recordLoginState(serverID string, id networkid.UserLoginID, state status.BridgeStateEvent) {
	c.loginStatesLock.Lock()
	defer c.loginStatesLock.Unlock()
	if c.loginStates == nil {
		c.loginStates = make(map[networkid.UserLoginID]loginState)
	}
	if state == "" {
		delete(c.loginStates, id)
	} else {
		c.loginStates[id] = loginState{serverID, state}
	}
	// Every configured server gets a full set of series, so a server whose
	// last login went away drops to zero instead of keeping stale counts.
	counts := make(map[loginState]int)
	servers := make(map[string]bool, len(c.Config.Servers))
	for _, srv := range c.Config.Servers {
		servers[srv.ID] = true
	}
	for _, s := range c.loginStates {
		counts[s]++
		servers[s.server] = true
	}
	for srv := range servers {
		for _, s := range []status.BridgeStateEvent{
			status.StateConnecting, status.StateConnected, status.StateTransientDisconnect,
			status.StateBadCredentials, status.StateUnknownError, status.StateLoggedOut,
		} {
			bridgemetrics.Logins.WithLabelValues(srv, string(s)).Set(float64(counts[loginState{srv, s}]))
		}
	}
}

var (
	_ bridgev2.NetworkConnector            = (*Connector)(nil)
	_ bridgev2.ConfigValidatingNetwork     = (*Connector)(nil)
	_ bridgev2.StoppableNetwork            = (*Connector)(nil)
	_ bridgev2.MaxFileSizeingNetwork       = (*Connector)(nil)
	_ bridgev2.IdentifierValidatingNetwork = (*Connector)(nil)
)

func (c *Connector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "Chatto",
		NetworkURL:       "https://chatto.run",
		NetworkIcon:      "",
		NetworkID:        "chatto",
		BeeperBridgeType: "github.com/elsbrock/mautrix-chatto",
		DefaultPort:      29340,
	}
}

func (c *Connector) Init(bridge *bridgev2.Bridge) {
	c.Bridge = bridge
	c.echoes = newEchoTracker()
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if proc, ok := bridge.Commands.(*commands.Processor); ok {
		proc.AddHandlers(cmdGrantBotPermissions, cmdAcceptInvite, cmdServerInfo)
	}
}

func (c *Connector) userAgent() string {
	if c.Version == "" {
		return "mautrix-chatto"
	}
	return "mautrix-chatto/" + c.Version
}

// newClient creates an API client for a server.
func (c *Connector) newClient(srv *server, tokens chattoclient.TokenSource) (*chattoclient.Client, error) {
	return chattoclient.New(srv.URL, chattoclient.Options{
		HTTPClient: c.HTTPClient,
		UserAgent:  c.userAgent(),
		Tokens:     tokens,
		Server:     srv.ID,
	})
}

func (c *Connector) Start(ctx context.Context) error {
	log := zerolog.Ctx(ctx)
	if err := c.Config.Validate(); err != nil {
		return err
	}
	bridgemetrics.BuildInfo.WithLabelValues(c.Version, "", chattoclient.UpstreamVersion).Set(1)
	if c.Config.Metrics.Enabled {
		if err := c.startMetrics(); err != nil {
			return err
		}
	}
	for _, id := range c.serverIDs() {
		if err := c.checkServerVersion(ctx, c.server(id)); err != nil {
			// The server may be down temporarily; logins will retry on their own.
			log.Warn().Err(err).Str("server", id).Msg("Failed to fetch Chatto server version")
		}
	}
	c.registerOAuthRoutes()
	c.setReady(true)
	return nil
}

func (c *Connector) Stop() {
	c.setReady(false)
	if c.metricsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.metricsServer.Shutdown(ctx)
	}
}

func (c *Connector) setReady(ready bool) {
	c.readyLock.Lock()
	c.ready = ready
	c.readyLock.Unlock()
}

func (c *Connector) isReady() bool {
	c.readyLock.RLock()
	defer c.readyLock.RUnlock()
	return c.ready
}

func (c *Connector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

func (c *Connector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal:    func() any { return &chattoid.PortalMetadata{} },
		Ghost:     func() any { return &chattoid.GhostMetadata{} },
		Message:   func() any { return &chattoid.MessageMetadata{} },
		Reaction:  nil,
		UserLogin: func() any { return &chattoid.UserLoginMetadata{} },
	}
}

func (c *Connector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		Provisioning: bridgev2.ProvisioningCapabilities{
			ResolveIdentifier: bridgev2.ResolveIdentifierCapabilities{
				CreateDM:       true,
				LookupUsername: true,
				ContactList:    true,
				Search:         true,
			},
		},
	}
}

func (c *Connector) SetMaxFileSize(maxSize int64) {
	maxFileSize = maxSize
}

func (c *Connector) ValidateUserID(id networkid.UserID) bool {
	srv, userID := chattoid.ParseUserID(id)
	return c.server(srv) != nil && validChattoID(userID, 'U')
}

// validChattoID reports whether s looks like a Chatto NanoID with the given
// type prefix (R rooms, E events, U users, G groups).
func validChattoID(s string, prefix byte) bool {
	if len(s) < 2 || len(s) > 32 || s[0] != prefix {
		return false
	}
	for i := 1; i < len(s); i++ {
		ch := s[i]
		isAlnum := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if !isAlnum && ch != '_' && ch != '-' {
			return false
		}
	}
	return true
}
