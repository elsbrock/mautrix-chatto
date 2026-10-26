package connector

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
)

// OAuth with Chatto (ADR 0007): Chatto has no client registration. The
// client_id is the HTTPS URL of a client metadata document, which the bridge
// serves from its public appservice address together with the redirect URI.

const (
	LoginFlowOAuth = "oauth"

	oauthMetadataPath = "/chatto/oauth/client.json"
	oauthCallbackPath = "/chatto/oauth/callback"
	loginStepOAuth    = "run.chatto.login.oauth"
	oauthLoginTimeout = 10 * time.Minute
)

var (
	ErrOAuthTimedOut = bridgev2.RespError{
		ErrCode:    "RUN.CHATTO.OAUTH_TIMED_OUT",
		Err:        "The Chatto sign-in was not completed in time, please start a new login",
		StatusCode: http.StatusGone,
	}
	ErrOAuthDenied = bridgev2.RespError{
		ErrCode:    "RUN.CHATTO.OAUTH_DENIED",
		Err:        "Chatto sign-in was denied",
		StatusCode: http.StatusForbidden,
	}
)

type oauthResult struct {
	code string
	err  error
}

// oauthState tracks pending authorizations by their state parameter.
type oauthState struct {
	mu      sync.Mutex
	pending map[string]chan oauthResult
}

// publicAddress returns the bridge's public HTTPS address, or "".
func (c *Connector) publicAddress() string {
	ms, ok := c.Bridge.Matrix.(bridgev2.MatrixConnectorWithServer)
	if !ok {
		return ""
	}
	addr := strings.TrimRight(ms.GetPublicAddress(), "/")
	if !strings.HasPrefix(addr, "https://") && !c.allowInsecureOAuth {
		return ""
	}
	return addr
}

func (c *Connector) oauthEnabled() bool {
	return c.publicAddress() != ""
}

func (c *Connector) oauthClientID() string { return c.publicAddress() + oauthMetadataPath }

func (c *Connector) oauthRedirectURI() string { return c.publicAddress() + oauthCallbackPath }

// registerOAuthRoutes serves the client metadata document and the callback.
func (c *Connector) registerOAuthRoutes() {
	ms, ok := c.Bridge.Matrix.(bridgev2.MatrixConnectorWithServer)
	if !ok || !c.oauthEnabled() {
		return
	}
	router := ms.GetRouter()
	if router == nil {
		return
	}
	c.oauth.pending = make(map[string]chan oauthResult)
	router.HandleFunc("GET "+oauthMetadataPath, c.serveOAuthMetadata)
	router.HandleFunc("GET "+oauthCallbackPath, c.serveOAuthCallback)
}

func (c *Connector) serveOAuthMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id":                  c.oauthClientID(),
		"client_name":                "Matrix bridge (mautrix-chatto)",
		"client_uri":                 "https://github.com/elsbrock/matrix-chatto-bridge",
		"redirect_uris":              []string{c.oauthRedirectURI()},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"application_type":           "web",
	})
}

func (c *Connector) serveOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	c.oauth.mu.Lock()
	ch, ok := c.oauth.pending[state]
	delete(c.oauth.pending, state)
	c.oauth.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, "<p>This sign-in link is unknown or expired. Start a new login from Matrix.</p>")
		return
	}
	result := oauthResult{code: q.Get("code")}
	if e := q.Get("error"); e != "" || result.code == "" {
		result.err = fmt.Errorf("%w: %s", errors.New(ErrOAuthDenied.Err), html.EscapeString(e))
	}
	ch <- result
	if result.err != nil {
		_, _ = fmt.Fprint(w, "<p>Sign-in was not completed. You can close this tab.</p>")
		return
	}
	_, _ = fmt.Fprint(w, "<p>Signed in to Chatto. You can close this tab and return to Matrix.</p>")
}

// OAuthLogin is the browser-based login flow.
type OAuthLogin struct {
	User     *bridgev2.User
	Main     *Connector
	state    string
	verifier string
	result   chan oauthResult
}

var _ bridgev2.LoginProcessDisplayAndWait = (*OAuthLogin)(nil)

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthorizeURL returns the Chatto authorization URL for this login.
func (o *OAuthLogin) AuthorizeURL() string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.Main.oauthClientID()},
		"redirect_uri":          {o.Main.oauthRedirectURI()},
		"state":                 {o.state},
		"code_challenge":        {pkceChallenge(o.verifier)},
		"code_challenge_method": {"S256"},
	}
	base, _ := chattoclient.ParseServerURL(o.Main.Config.ServerURL)
	return base.String() + "/oauth/authorize?" + q.Encode()
}

func (o *OAuthLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	o.state = randomToken(24)
	o.verifier = randomToken(48)
	o.result = make(chan oauthResult, 1)
	o.Main.oauth.mu.Lock()
	o.Main.oauth.pending[o.state] = o.result
	o.Main.oauth.mu.Unlock()
	link := o.AuthorizeURL()
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeDisplayAndWait,
		StepID:       loginStepOAuth,
		Instructions: "Open this link, sign in to Chatto and approve the bridge:\n\n" + link,
		DisplayAndWaitParams: &bridgev2.LoginDisplayAndWaitParams{
			Type:      bridgev2.LoginDisplayTypeCode,
			Data:      link,
			CanCancel: true,
		},
	}, nil
}

func (o *OAuthLogin) Wait(ctx context.Context) (*bridgev2.LoginStep, error) {
	timer := time.NewTimer(oauthLoginTimeout)
	defer timer.Stop()
	var res oauthResult
	select {
	case res = <-o.result:
	case <-timer.C:
		o.Cancel()
		return nil, ErrOAuthTimedOut
	case <-ctx.Done():
		o.Cancel()
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, ErrOAuthDenied
	}
	client, err := o.Main.newClient(nil)
	if err != nil {
		return nil, err
	}
	sess, err := client.ExchangeAuthorizationCode(ctx, res.code, o.verifier, o.Main.oauthRedirectURI(), o.Main.oauthClientID())
	if err != nil {
		return nil, fmt.Errorf("failed to exchange authorization code: %w", err)
	}
	return o.Main.finishLogin(ctx, o.User, &chattoid.UserLoginMetadata{Kind: chattoid.CredentialSession, Session: sess})
}

func (o *OAuthLogin) Cancel() {
	o.Main.oauth.mu.Lock()
	delete(o.Main.oauth.pending, o.state)
	o.Main.oauth.mu.Unlock()
}
