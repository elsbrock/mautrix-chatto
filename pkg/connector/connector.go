// Package connector implements the bridgev2 network connector for Chatto.
package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/bridgemetrics"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
)

// Connector is the bridgev2 NetworkConnector for one Chatto server.
type Connector struct {
	Bridge *bridgev2.Bridge
	Config Config

	// HTTPClient is used for all Chatto requests. Tests replace it.
	HTTPClient *http.Client
	// Version is the bridge version, used in the user agent.
	Version string

	serverVersion string
	metricsServer *http.Server
	uploadLimit   uploadLimit

	readyLock sync.RWMutex
	ready     bool

	loginStatesLock sync.Mutex
	loginStates     map[networkid.UserLoginID]status.BridgeStateEvent
}

// recordLoginState updates the per-state login gauge.
func (c *Connector) recordLoginState(id networkid.UserLoginID, state status.BridgeStateEvent) {
	c.loginStatesLock.Lock()
	defer c.loginStatesLock.Unlock()
	if c.loginStates == nil {
		c.loginStates = make(map[networkid.UserLoginID]status.BridgeStateEvent)
	}
	if state == "" {
		delete(c.loginStates, id)
	} else {
		c.loginStates[id] = state
	}
	counts := make(map[status.BridgeStateEvent]int)
	for _, s := range c.loginStates {
		counts[s]++
	}
	for _, s := range []status.BridgeStateEvent{
		status.StateConnecting, status.StateConnected, status.StateTransientDisconnect,
		status.StateBadCredentials, status.StateUnknownError, status.StateLoggedOut,
	} {
		bridgemetrics.Logins.WithLabelValues(string(s)).Set(float64(counts[s]))
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
		BeeperBridgeType: "github.com/elsbrock/matrix-chatto-bridge",
		DefaultPort:      29340,
	}
}

func (c *Connector) Init(bridge *bridgev2.Bridge) {
	c.Bridge = bridge
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

// newClient creates an API client for this connector's server.
func (c *Connector) newClient(tokens chattoclient.TokenSource) (*chattoclient.Client, error) {
	return chattoclient.New(c.Config.ServerURL, chattoclient.Options{
		HTTPClient: c.HTTPClient,
		UserAgent:  c.userAgent(),
		Tokens:     tokens,
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
	client, err := c.newClient(nil)
	if err != nil {
		return err
	}
	version, err := client.ServerVersion(ctx)
	if err != nil {
		// The server may be down temporarily; logins will retry on their own.
		log.Warn().Err(err).Msg("Failed to fetch Chatto server version")
	} else {
		c.serverVersion = version
		supported := CheckServerVersion(version)
		bridgemetrics.ServerInfo.WithLabelValues(version, fmt.Sprint(supported)).Set(1)
		if !supported {
			msg := fmt.Sprintf("Chatto server version %s is older than %s, which this bridge was built against", version, chattoclient.UpstreamVersion)
			if c.Config.StrictServerVersion {
				return errors.New(msg)
			}
			log.Warn().Str("server_version", version).Msg(msg)
		} else {
			log.Info().Str("server_version", version).Msg("Connected to Chatto server")
		}
	}
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
	return validChattoID(string(id), 'U')
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
