// Package chattoclient is a Go client for Chatto's ConnectRPC API, its HTTP
// auth endpoints and its realtime WebSocket protocol.
package chattoclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/elsbrock/mautrix-chatto/pkg/bridgemetrics"
	"github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/admin/v1/adminv1connect"
	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
	"github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1/apiv1connect"
	discoveryv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/discovery/v1"
	"github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/discovery/v1/discoveryv1connect"
)

// UpstreamVersion is the Chatto release whose protos are vendored in proto/.
// scripts/sync-proto.sh reads it; keep it the single source of truth.
const UpstreamVersion = "v0.5.0-beta.21"

// connectPrefix is where Chatto mounts its ConnectRPC handlers.
const connectPrefix = "/api/connect"

// Client talks to one Chatto server as one account.
type Client struct {
	base      *url.URL
	http      *http.Client
	userAgent string
	tokens    TokenSource
	now       func() time.Time
	server    string

	Discovery        discoveryv1connect.ServerDiscoveryServiceClient
	Viewer           apiv1connect.ViewerServiceClient
	Account          apiv1connect.MyAccountServiceClient
	Server           apiv1connect.ServerServiceClient
	Users            apiv1connect.UserServiceClient
	Rooms            apiv1connect.RoomServiceClient
	RoomDirectory    apiv1connect.RoomDirectoryServiceClient
	RoomInvitations  apiv1connect.RoomInvitationServiceClient
	Threads          apiv1connect.ThreadServiceClient
	Messages         apiv1connect.MessageServiceClient
	Assets           apiv1connect.AssetServiceClient
	AssetUploads     apiv1connect.AssetUploadServiceClient
	VoiceCalls       apiv1connect.VoiceCallServiceClient
	Bots             apiv1connect.BotServiceClient
	Permissions      apiv1connect.PermissionServiceClient
	AdminPermissions adminv1connect.AdminPermissionServiceClient
}

// Options configures a Client.
type Options struct {
	HTTPClient *http.Client
	UserAgent  string
	// Tokens supplies credentials. Nil means unauthenticated.
	Tokens TokenSource
	// Now overrides the clock, for tests.
	Now func() time.Time
	// Server labels the client's metrics with the configured server ID.
	Server string
}

// New creates a client for the server at baseURL.
func New(baseURL string, opts Options) (*Client, error) {
	base, err := ParseServerURL(baseURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		base:      base,
		http:      opts.HTTPClient,
		userAgent: opts.UserAgent,
		tokens:    opts.Tokens,
		now:       opts.Now,
		server:    opts.Server,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 2 * time.Minute}
	}
	if c.userAgent == "" {
		c.userAgent = "mautrix-chatto"
	}
	if c.now == nil {
		c.now = time.Now
	}
	connectURL := strings.TrimRight(base.String(), "/") + connectPrefix
	copts := []connect.ClientOption{connect.WithInterceptors(&interceptor{client: c})}
	c.Discovery = discoveryv1connect.NewServerDiscoveryServiceClient(c.http, connectURL, copts...)
	c.Viewer = apiv1connect.NewViewerServiceClient(c.http, connectURL, copts...)
	c.Account = apiv1connect.NewMyAccountServiceClient(c.http, connectURL, copts...)
	c.Server = apiv1connect.NewServerServiceClient(c.http, connectURL, copts...)
	c.Users = apiv1connect.NewUserServiceClient(c.http, connectURL, copts...)
	c.Rooms = apiv1connect.NewRoomServiceClient(c.http, connectURL, copts...)
	c.RoomDirectory = apiv1connect.NewRoomDirectoryServiceClient(c.http, connectURL, copts...)
	c.RoomInvitations = apiv1connect.NewRoomInvitationServiceClient(c.http, connectURL, copts...)
	c.Threads = apiv1connect.NewThreadServiceClient(c.http, connectURL, copts...)
	c.Messages = apiv1connect.NewMessageServiceClient(c.http, connectURL, copts...)
	c.Assets = apiv1connect.NewAssetServiceClient(c.http, connectURL, copts...)
	c.AssetUploads = apiv1connect.NewAssetUploadServiceClient(c.http, connectURL, copts...)
	c.VoiceCalls = apiv1connect.NewVoiceCallServiceClient(c.http, connectURL, copts...)
	c.Bots = apiv1connect.NewBotServiceClient(c.http, connectURL, copts...)
	c.Permissions = apiv1connect.NewPermissionServiceClient(c.http, connectURL, copts...)
	c.AdminPermissions = adminv1connect.NewAdminPermissionServiceClient(c.http, connectURL, copts...)
	return c, nil
}

// WithTokens returns a copy of c that authenticates with tokens.
func (c *Client) WithTokens(tokens TokenSource) *Client {
	clone, _ := New(c.base.String(), Options{HTTPClient: c.http, UserAgent: c.userAgent, Tokens: tokens, Now: c.now, Server: c.server})
	return clone
}

// ParseServerURL normalises a user-supplied server address. A bare host gets
// https://, and any path, query or fragment is dropped.
func ParseServerURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("chatto: empty server URL")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("chatto: invalid server URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("chatto: unsupported server URL scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("chatto: server URL has no host")
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// BaseURL returns the server's base URL.
func (c *Client) BaseURL() *url.URL { u := *c.base; return &u }

// HTTP returns the underlying HTTP client.
func (c *Client) HTTP() *http.Client { return c.http }

// Tokens returns the client's token source.
func (c *Client) Tokens() TokenSource { return c.tokens }

func (c *Client) url(path string) string {
	return strings.TrimRight(c.base.String(), "/") + path
}

// ResolveURL makes a possibly relative URL returned by Chatto absolute.
func (c *Client) ResolveURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return c.base.ResolveReference(u).String()
}

// ServerVersion returns the version reported by the server's public profile.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	profile, err := c.ServerProfile(ctx)
	if err != nil {
		return "", err
	}
	return profile.GetVersion(), nil
}

// ServerProfile returns the server's public profile. It needs no login.
func (c *Client) ServerProfile(ctx context.Context) (*apiv1.ServerPublicProfile, error) {
	resp, err := c.Discovery.GetServer(ctx, connect.NewRequest(&discoveryv1.GetServerRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetProfile(), nil
}

// tokenError maps a TokenSource failure to a connect error. Only a revoked
// session is an authentication failure; anything else (network trouble, a
// failing token endpoint, a failed save) is transient.
func tokenError(err error) error {
	if errors.Is(err, ErrSessionRevoked) {
		return connect.NewError(connect.CodeUnauthenticated, err)
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("refreshing credentials: %w", err))
}

// interceptor adds credentials, retries once after a refreshable auth
// failure, and records metrics.
type interceptor struct {
	client *Client
}

func (i *interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		req.Header().Set("User-Agent", i.client.userAgent)
		var token string
		if i.client.tokens != nil {
			var err error
			token, err = i.client.tokens.Token(ctx)
			if err != nil {
				return nil, tokenError(err)
			}
			req.Header().Set("Authorization", "Bearer "+token)
		}
		resp, err := next(ctx, req)
		if err != nil && connect.CodeOf(err) == connect.CodeUnauthenticated && i.client.tokens != nil {
			if _, static := i.client.tokens.(StaticToken); !static {
				i.client.tokens.Invalidate(token)
				if fresh, terr := i.client.tokens.Token(ctx); terr == nil && fresh != token {
					req.Header().Set("Authorization", "Bearer "+fresh)
					resp, err = next(ctx, req)
				}
			}
		}
		code := "ok"
		if err != nil {
			code = connect.CodeOf(err).String()
			bridgemetrics.RPCErrors.WithLabelValues(i.client.server, req.Spec().Procedure, code).Inc()
		}
		bridgemetrics.RPCDuration.WithLabelValues(i.client.server, req.Spec().Procedure, code).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

func (i *interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
