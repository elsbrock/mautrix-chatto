package chattoclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Session is a Chatto bearer session as issued by password login or OAuth.
// Refresh tokens are single use: every refresh returns a new pair, and the
// caller must persist it before using the next one.
type Session struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	AccessExpiry  time.Time `json:"access_expiry"`
	RefreshExpiry time.Time `json:"refresh_expiry,omitzero"`
	// ClientID is the OAuth client the session belongs to. It is empty for
	// password-login sessions and must be echoed on refresh.
	ClientID string `json:"client_id,omitempty"`
	// PendingRefreshID is the request ID of a refresh whose response may have
	// been lost. Retrying with the same ID and the previous refresh token lets
	// the server replay the response instead of revoking the session.
	PendingRefreshID string `json:"pending_refresh_id,omitempty"`
}

// LoginResult is the outcome of a password login.
type LoginResult struct {
	Session Session
	UserID  string
	Login   string
}

// Errors returned by the HTTP auth endpoints.
var (
	ErrInvalidCredentials = errors.New("chatto: invalid credentials")
	ErrDirectLoginOff     = errors.New("chatto: password login is disabled on this server")
	ErrSessionRevoked     = errors.New("chatto: session expired or revoked")
)

// HTTPError is an unexpected HTTP response from an auth endpoint.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("chatto: unexpected HTTP %d: %s", e.StatusCode, e.Body)
}

func (c *Client) postJSON(ctx context.Context, path, bearer string, reqBody, respBody any) (int, []byte, error) {
	data, err := json.Marshal(reqBody)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(path), bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode == http.StatusOK && respBody != nil {
		if err := json.Unmarshal(raw, respBody); err != nil {
			return resp.StatusCode, raw, fmt.Errorf("chatto: decoding %s response: %w", path, err)
		}
	}
	return resp.StatusCode, raw, nil
}

// PasswordLogin exchanges a login (username or email) and password for a
// bearer session. It never requests a cookie session.
func (c *Client) PasswordLogin(ctx context.Context, login, password string) (*LoginResult, error) {
	var resp struct {
		Success bool `json:"success"`
		User    struct {
			ID    string `json:"id"`
			Login string `json:"login"`
		} `json:"user"`
		Token                 string `json:"token"`
		RefreshToken          string `json:"refreshToken"`
		ExpiresIn             int64  `json:"expiresIn"`
		RefreshTokenExpiresIn int64  `json:"refreshTokenExpiresIn"`
	}
	status, raw, err := c.postJSON(ctx, "/auth/login", "", map[string]string{"login": login, "password": password}, &resp)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, ErrInvalidCredentials
	case http.StatusForbidden:
		return nil, ErrDirectLoginOff
	default:
		return nil, &HTTPError{StatusCode: status, Body: truncate(string(raw), 200)}
	}
	if resp.Token == "" {
		return nil, fmt.Errorf("chatto: login response has no bearer token")
	}
	now := c.now()
	result := &LoginResult{
		UserID: resp.User.ID,
		Login:  resp.User.Login,
		Session: Session{
			AccessToken:  resp.Token,
			RefreshToken: resp.RefreshToken,
			AccessExpiry: now.Add(time.Duration(resp.ExpiresIn) * time.Second),
		},
	}
	if resp.RefreshTokenExpiresIn > 0 {
		result.Session.RefreshExpiry = now.Add(time.Duration(resp.RefreshTokenExpiresIn) * time.Second)
	}
	return result, nil
}

// RefreshSession rotates a session's tokens. The returned session replaces
// the old one; the old refresh token must never be presented again.
//
// Before calling, persist sess with PendingRefreshID set (PrepareRefresh) so
// that a crash after the server rotated the token can be recovered.
func (c *Client) RefreshSession(ctx context.Context, sess Session) (*Session, error) {
	if sess.RefreshToken == "" {
		return nil, ErrSessionRevoked
	}
	requestID := sess.PendingRefreshID
	if requestID == "" {
		requestID = uuid.NewString()
	}
	body := map[string]string{
		"grant_type":         "refresh_token",
		"refresh_token":      sess.RefreshToken,
		"refresh_request_id": requestID,
	}
	if sess.ClientID != "" {
		body["client_id"] = sess.ClientID
	}
	var resp struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	status, raw, err := c.postJSON(ctx, "/oauth/token", "", body, &resp)
	if err != nil {
		return nil, err
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized {
		var oauthErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &oauthErr)
		if oauthErr.Error == "invalid_grant" || status == http.StatusUnauthorized {
			return nil, ErrSessionRevoked
		}
		return nil, &HTTPError{StatusCode: status, Body: truncate(string(raw), 200)}
	} else if status != http.StatusOK {
		return nil, &HTTPError{StatusCode: status, Body: truncate(string(raw), 200)}
	}
	now := c.now()
	next := &Session{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		AccessExpiry: now.Add(time.Duration(resp.ExpiresIn) * time.Second),
		ClientID:     sess.ClientID,
	}
	if resp.RefreshTokenExpiresIn > 0 {
		next.RefreshExpiry = now.Add(time.Duration(resp.RefreshTokenExpiresIn) * time.Second)
	}
	return next, nil
}

// ExchangeAuthorizationCode completes an OAuth authorization code flow with
// PKCE. clientID is the client metadata document URL.
func (c *Client) ExchangeAuthorizationCode(ctx context.Context, code, verifier, redirectURI, clientID string) (*Session, error) {
	var resp struct {
		AccessToken           string `json:"access_token"`
		ExpiresIn             int64  `json:"expires_in"`
		RefreshToken          string `json:"refresh_token"`
		RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	status, raw, err := c.postJSON(ctx, "/oauth/token", "", map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"code_verifier": verifier,
		"redirect_uri":  redirectURI,
		"client_id":     clientID,
	}, &resp)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, &HTTPError{StatusCode: status, Body: truncate(string(raw), 200)}
	}
	now := c.now()
	sess := &Session{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		AccessExpiry: now.Add(time.Duration(resp.ExpiresIn) * time.Second),
		ClientID:     clientID,
	}
	if resp.RefreshTokenExpiresIn > 0 {
		sess.RefreshExpiry = now.Add(time.Duration(resp.RefreshTokenExpiresIn) * time.Second)
	}
	return sess, nil
}

// PrepareRefresh returns sess with a fresh PendingRefreshID unless one is
// already pending.
func PrepareRefresh(sess Session) Session {
	if sess.PendingRefreshID == "" {
		sess.PendingRefreshID = uuid.NewString()
	}
	return sess
}

// Logout revokes the session on the server. Errors are informational: the
// bridge forgets the tokens either way.
func (c *Client) Logout(ctx context.Context, sess Session) error {
	status, raw, err := c.postJSON(ctx, "/auth/logout", sess.AccessToken, map[string]string{"refreshToken": sess.RefreshToken}, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusUnauthorized {
		return &HTTPError{StatusCode: status, Body: truncate(string(raw), 200)}
	}
	return nil
}

// IsBotAPIKey reports whether token is a bot API key rather than a session
// access token.
func IsBotAPIKey(token string) bool {
	return strings.HasPrefix(token, "cht_BK_")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
