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

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
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
)

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
		Instructions: fmt.Sprintf("Log in to %s", p.Main.Config.ServerURL),
		UserInputParams: &bridgev2.LoginUserInputParams{
			Fields: []bridgev2.LoginInputDataField{
				{Type: bridgev2.LoginInputFieldTypeUsername, ID: "login", Name: "Username or email"},
				{Type: bridgev2.LoginInputFieldTypePassword, ID: "password", Name: "Password"},
			},
		},
	}, nil
}

func (p *PasswordLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	client, err := p.Main.newClient(nil)
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
	return p.Main.finishLogin(ctx, p.User, meta)
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
			Fields: []bridgev2.LoginInputDataField{
				{Type: bridgev2.LoginInputFieldTypeToken, ID: "token", Name: "Bot API key or refresh token"},
			},
		},
	}, nil
}

func (t *TokenLogin) SubmitUserInput(ctx context.Context, input map[string]string) (*bridgev2.LoginStep, error) {
	token := strings.TrimSpace(input["token"])
	if token == "" {
		return nil, ErrInvalidToken
	}
	meta := &chattoid.UserLoginMetadata{}
	if chattoclient.IsBotAPIKey(token) {
		meta.Kind = chattoid.CredentialBotKey
		meta.BotKey = token
	} else {
		client, err := t.Main.newClient(nil)
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
	return t.Main.finishLogin(ctx, t.User, meta)
}

func (t *TokenLogin) Cancel() {}

// finishLogin verifies the credentials, creates the UserLogin and connects it.
func (c *Connector) finishLogin(ctx context.Context, user *bridgev2.User, meta *chattoid.UserLoginMetadata) (*bridgev2.LoginStep, error) {
	var tokens chattoclient.TokenSource
	var sessionSource *chattoclient.SessionTokenSource
	client, err := c.newClient(nil)
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
	remoteName := me.GetLogin()
	if me.GetDisplayName() != "" {
		remoteName = fmt.Sprintf("%s (@%s)", me.GetDisplayName(), me.GetLogin())
	}
	ul, err := user.NewLogin(ctx, &database.UserLogin{
		ID:         chattoid.MakeUserLoginID(me.GetId()),
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
	base, err := c.newClient(nil)
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
	login.Client = cc
	return nil
}
