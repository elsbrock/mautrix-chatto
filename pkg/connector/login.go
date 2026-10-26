package connector

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
)

const (
	LoginFlowPassword = "password"
	LoginFlowToken    = "token"

	loginStepPassword = "run.chatto.login.password"
	loginStepToken    = "run.chatto.login.token"
	loginStepComplete = "run.chatto.login.complete"
)

var (
	ErrInvalidCredentials = bridgev2.RespError{
		ErrCode:    "RUN.CHATTO.INVALID_CREDENTIALS",
		Err:        "Invalid username or password",
		StatusCode: http.StatusForbidden,
	}
	ErrPasswordLoginDisabled = bridgev2.RespError{
		ErrCode:    "RUN.CHATTO.PASSWORD_LOGIN_DISABLED",
		Err:        "Password login is disabled on this Chatto server or bridge, use a token instead",
		StatusCode: http.StatusForbidden,
	}
	ErrInvalidToken = bridgev2.RespError{
		ErrCode:    "RUN.CHATTO.INVALID_TOKEN",
		Err:        "Chatto rejected the token",
		StatusCode: http.StatusForbidden,
	}
	ErrUnknownServer = bridgev2.RespError{
		ErrCode:    "RUN.CHATTO.UNKNOWN_SERVER",
		Err:        "That Chatto server is not configured on this bridge",
		StatusCode: http.StatusBadRequest,
	}
)

// serverFieldID is the login input field that picks the server.
const serverFieldID = "server"

// serverFields returns the server picker for login steps. It is empty when
// only one server is configured.
func (c *Connector) serverFields() []bridgev2.LoginInputDataField {
	ids := c.serverIDs()
	if len(ids) < 2 {
		return nil
	}
	return []bridgev2.LoginInputDataField{{
		Type:    bridgev2.LoginInputFieldTypeSelect,
		ID:      serverFieldID,
		Name:    "Chatto server",
		Options: ids,
		Pattern: "^(" + strings.Join(ids, "|") + ")$",
	}}
}

// loginServer returns the server picked in a login step, or the only one.
func (c *Connector) loginServer(input map[string]string) (*server, error) {
	ids := c.serverIDs()
	if len(ids) == 1 {
		return c.server(ids[0]), nil
	}
	if srv := c.server(strings.TrimSpace(input[serverFieldID])); srv != nil {
		return srv, nil
	}
	return nil, ErrUnknownServer
}

// serverNames describes the servers a login step can reach.
func (c *Connector) serverNames() string {
	urls := make([]string, len(c.Config.Servers))
	for i, srv := range c.Config.Servers {
		urls[i] = fmt.Sprintf("%s (%s)", srv.ID, srv.URL)
	}
	return strings.Join(urls, ", ")
}

func (c *Connector) GetLoginFlows() []bridgev2.LoginFlow {
	var flows []bridgev2.LoginFlow
	if c.Config.AllowPasswordLogin {
		flows = append(flows, bridgev2.LoginFlow{
			Name:        "Password",
			Description: "Log in with your Chatto username (or email) and password",
			ID:          LoginFlowPassword,
		})
	}
	if c.oauthEnabled() {
		flows = append(flows, bridgev2.LoginFlow{
			Name:        "Browser",
			Description: "Sign in to Chatto in your browser (OAuth)",
			ID:          LoginFlowOAuth,
		})
	}
	flows = append(flows, bridgev2.LoginFlow{
		Name:        "Token",
		Description: "Log in with a bot API key (cht_BK_…) or a session refresh token",
		ID:          LoginFlowToken,
	})
	return flows
}

func (c *Connector) CreateLogin(ctx context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	switch flowID {
	case LoginFlowPassword:
		if !c.Config.AllowPasswordLogin {
			return nil, bridgev2.ErrInvalidLoginFlowID
		}
		return &PasswordLogin{User: user, Main: c}, nil
	case LoginFlowToken:
		return &TokenLogin{User: user, Main: c}, nil
	case LoginFlowOAuth:
		if !c.oauthEnabled() || c.oauth.pending == nil {
			return nil, bridgev2.ErrInvalidLoginFlowID
		}
		return &OAuthLogin{User: user, Main: c}, nil
	}
	return nil, bridgev2.ErrInvalidLoginFlowID
}

// PasswordLogin logs in with a username and password.
type PasswordLogin struct {
	User *bridgev2.User
	Main *Connector
}

var _ bridgev2.LoginProcessUserInput = (*PasswordLogin)(nil)

func (p *PasswordLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:         bridgev2.LoginStepTypeUserInput,
		StepID:       loginStepPassword,
		Instructions: fmt.Sprintf("Log in to %s", p.Main.serverNames()),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: append(p.Main.serverFields(),
				bridgev2.LoginInputDataField{Type: bridgev2.LoginInputFieldTypeUsername, ID: "login", Name: "Username or email"},
				bridgev2.LoginInputDataField{Type: bridgev2.LoginInputFieldTypePassword, ID: "password", Name: "Password"},
			),
		},
	}, nil
}

func (p *PasswordLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	srv, err := p.Main.loginServer(input)
	if err != nil {
		return nil, err
	}
	client, err := p.Main.newClient(srv, nil)
	if err != nil {
		return nil, err
	}
	result, err := client.PasswordLogin(ctx, strings.TrimSpace(input["login"]), input["password"])
	switch {
	case errors.Is(err, chattoclient.ErrInvalidCredentials):
		return nil, ErrInvalidCredentials
	case errors.Is(err, chattoclient.ErrDirectLoginOff):
		return nil, ErrPasswordLoginDisabled
	case err != nil:
		return nil, fmt.Errorf("login failed: %w", err)
	}
	meta := &chattoid.UserLoginMetadata{
		Kind:    chattoid.CredentialSession,
		Session: &result.Session,
		Login:   result.Login,
	}
	return p.Main.finishLogin(ctx, p.User, srv, meta)
}

func (p *PasswordLogin) Cancel() {}

// TokenLogin logs in with a bot API key or a session refresh token.
type TokenLogin struct {
	User *bridgev2.User
	Main *Connector
}

var _ bridgev2.LoginProcessUserInput = (*TokenLogin)(nil)

func (t *TokenLogin) Start(ctx context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: loginStepToken,
		Instructions: "Paste a Chatto bot API key (starts with cht_BK_), or the refresh token " +
			"(cht_RT_…) of an existing session. The bridge takes over a refresh token: " +
			"the client it came from will be signed out on its next refresh.",
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: append(t.Main.serverFields(),
				bridgev2.LoginInputDataField{Type: bridgev2.LoginInputFieldTypeToken, ID: "token", Name: "Bot API key or refresh token"},
			),
		},
	}, nil
}

func (t *TokenLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	srv, err := t.Main.loginServer(input)
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(input["token"])
	if token == "" {
		return nil, ErrInvalidToken
	}
	meta := &chattoid.UserLoginMetadata{}
	if chattoclient.IsBotAPIKey(token) {
		meta.Kind = chattoid.CredentialBotKey
		meta.BotKey = token
	} else {
		client, err := t.Main.newClient(srv, nil)
		if err != nil {
			return nil, err
		}
		sess, err := client.RefreshSession(ctx, chattoclient.Session{RefreshToken: token})
		if errors.Is(err, chattoclient.ErrSessionRevoked) {
			return nil, ErrInvalidToken
		} else if err != nil {
			return nil, fmt.Errorf("failed to redeem refresh token: %w", err)
		}
		meta.Kind = chattoid.CredentialSession
		meta.Session = sess
	}
	return t.Main.finishLogin(ctx, t.User, srv, meta)
}

func (t *TokenLogin) Cancel() {}

// finishLogin verifies the credentials, creates the UserLogin and connects it.
func (c *Connector) finishLogin(ctx context.Context, user *bridgev2.User, srv *server, meta *chattoid.UserLoginMetadata) (*bridgev2.LoginStep, error) {
	var tokens chattoclient.TokenSource
	var sessionSource *chattoclient.SessionTokenSource
	client, err := c.newClient(srv, nil)
	if err != nil {
		return nil, err
	}
	if meta.Kind == chattoid.CredentialBotKey {
		tokens = chattoclient.StaticToken(meta.BotKey)
	} else {
		sessionSource = chattoclient.NewSessionTokenSource(client, *meta.Session, nil)
		tokens = sessionSource
	}
	client = client.WithTokens(tokens)
	viewer, err := client.Viewer.GetViewer(ctx, connect.NewRequest(&apiv1.GetViewerRequest{}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			return nil, ErrInvalidToken
		}
		return nil, fmt.Errorf("failed to fetch Chatto account: %w", err)
	}
	if sessionSource != nil {
		// GetViewer may have refreshed the session; keep the newest tokens.
		sess := sessionSource.Session()
		meta.Session = &sess
	}
	me := viewer.Msg.GetUser().GetProfile()
	if me.GetId() == "" {
		return nil, fmt.Errorf("chatto returned an empty viewer")
	}
	meta.Login = me.GetLogin()
	meta.IsBot = me.GetBot() != nil
	meta.Server = srv.ID
	remoteName := me.GetLogin()
	if me.GetDisplayName() != "" {
		remoteName = fmt.Sprintf("%s (@%s)", me.GetDisplayName(), me.GetLogin())
	}
	if len(c.Config.Servers) > 1 {
		remoteName += " on " + srv.ID
	}
	ul, err := user.NewLogin(ctx, &database.UserLogin{
		ID:         chattoid.MakeUserLoginID(srv.ID, me.GetId()),
		RemoteName: remoteName,
		RemoteProfile: status.RemoteProfile{
			Username: me.GetLogin(),
			Name:     me.GetDisplayName(),
		},
		Metadata: meta,
	}, &bridgev2.NewLoginParams{DeleteOnConflict: true})
	if err != nil {
		return nil, fmt.Errorf("failed to save login: %w", err)
	}
	go ul.Client.Connect(ul.Log.WithContext(c.Bridge.BackgroundCtx))
	instructions := fmt.Sprintf("Logged in as %s", remoteName)
	if meta.IsBot {
		instructions += ". This is a bot account: it only sees rooms it was added to and needs explicit permissions, see `grant-bot-permissions`."
	}
	return &bridgev2.LoginStep{
		Type:           bridgev2.LoginStepTypeComplete,
		StepID:         loginStepComplete,
		Instructions:   instructions,
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: ul.ID, UserLogin: ul},
	}, nil
}

func (c *Connector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*chattoid.UserLoginMetadata)
	cc := newChattoClient(c, login)
	login.Client = cc
	if cc.srv == nil {
		// Keep the login loaded so it can be logged out; Connect reports why
		// it stays disconnected.
		login.Log.Warn().Str("server", cc.serverID).
			Msg("Login belongs to a Chatto server that is not in network.servers")
		return nil
	}
	base, err := c.newClient(cc.srv, nil)
	if err != nil {
		return err
	}
	switch meta.Kind {
	case chattoid.CredentialBotKey:
		cc.tokens = chattoclient.StaticToken(meta.BotKey)
	case chattoid.CredentialSession:
		if meta.Session == nil {
			cc.tokens = nil
			break
		}
		src := chattoclient.NewSessionTokenSource(base, *meta.Session, cc.persistSession)
		src.OnPersistError = func(err error) {
			login.Log.Err(err).Msg("Failed to persist rotated Chatto session; a restart will require logging in again")
		}
		cc.tokens = src
	}
	if cc.tokens != nil {
		cc.api = base.WithTokens(cc.tokens)
	}
	return nil
}
