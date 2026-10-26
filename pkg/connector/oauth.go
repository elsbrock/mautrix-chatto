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

// oauthPending is one authorization waiting for its callback.
type oauthPending struct {
	result chan oauthResult
	// mxid is the Matrix user that will receive the Chatto session. The
	// callback page names it so that someone who was sent another user's
	// sign-in link can see who they would be linking their account to.
	mxid string
}

// oauthState tracks pending authorizations by their state parameter.
type oauthState struct {
	mu      sync.Mutex
	pending map[string]*oauthPending
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
	c.oauth.pending = make(map[string]*oauthPending)
	router.HandleFunc("GET "+oauthMetadataPath, c.serveOAuthMetadata)
	router.HandleFunc("GET "+oauthCallbackPath, c.serveOAuthConfirm)
	router.HandleFunc("POST "+oauthCallbackPath, c.serveOAuthCallback)
}

func (c *Connector) serveOAuthMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"client_id":   c.oauthClientID(),
		"client_name": "Matrix bridge (mautrix-chatto)",
		// Chatto requires client_uri to share the client_id's origin.
		"client_uri":                 c.publicAddress(),
		"redirect_uris":              []string{c.oauthRedirectURI()},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"application_type":           "web",
	})
}

// serveOAuthConfirm is the redirect target. It does not complete the login
// on GET: it asks the person to confirm which Matrix account receives their
// Chatto session, then posts back.
func (c *Connector) serveOAuthConfirm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	c.oauth.mu.Lock()
	p, ok := c.oauth.pending[state]
	c.oauth.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; form-action 'self'; style-src 'unsafe-inline'")
	w.Header().Set("X-Frame-Options", "DENY")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, "<p>This sign-in link is unknown or expired. Start a new login from Matrix.</p>")
		return
	}
	if e := q.Get("error"); e != "" || q.Get("code") == "" {
		c.finishOAuth(state, oauthResult{err: errors.New(ErrOAuthDenied.Err)})
		_, _ = fmt.Fprint(w, "<p>Sign-in was not completed. You can close this tab.</p>")
		return
	}
	_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Link Chatto account</title>
<body style="font-family:sans-serif;max-width:32em;margin:3em auto">
<h1>Link your Chatto account?</h1>
<p>Your Chatto account will be bridged to the Matrix account <strong>%s</strong>.
Only continue if that is you.</p>
<form method="post"><input type="hidden" name="state" value="%s"><input type="hidden" name="code" value="%s">
<button type="submit">Link account</button></form></body>`,
		html.EscapeString(p.mxid), html.EscapeString(state), html.EscapeString(q.Get("code")))
}

func (c *Connector) serveOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !c.finishOAuth(r.PostForm.Get("state"), oauthResult{code: r.PostForm.Get("code")}) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, "<p>This sign-in link is unknown or expired. Start a new login from Matrix.</p>")
		return
	}
	_, _ = fmt.Fprint(w, "<p>Linked. You can close this tab and return to Matrix.</p>")
}

// finishOAuth delivers a result to the pending login, once.
func (c *Connector) finishOAuth(state string, res oauthResult) bool {
	c.oauth.mu.Lock()
	p, ok := c.oauth.pending[state]
	delete(c.oauth.pending, state)
	c.oauth.mu.Unlock()
	if !ok {
		return false
	}
	if res.err == nil && res.code == "" {
		res.err = errors.New(ErrOAuthDenied.Err)
	}
	p.result <- res
	return true
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
	o.Main.oauth.pending[o.state] = &oauthPending{result: o.result, mxid: string(o.User.MXID)}
	o.Main.oauth.mu.Unlock()
	link := o.AuthorizeURL()
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeDisplayAndWait,
		StepID:       loginStepOAuth,
		Instructions: fmt.Sprintf("Open this link, sign in to Chatto and approve linking it to %s:\n\n%s", o.User.MXID, link),
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
