package chattoclient

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/elsbrock/mautrix-chatto/pkg/bridgemetrics"
)

// TokenSource supplies the bearer credential for requests.
type TokenSource interface {
	// Token returns a currently valid bearer token, refreshing it if needed.
	Token(ctx context.Context) (string, error)
	// Invalidate tells the source that the server rejected token, so that the
	// next Token call refreshes instead of returning it again.
	Invalidate(token string)
}

// StaticToken is a non-expiring credential such as a bot API key.
type StaticToken string

func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }
func (t StaticToken) Invalidate(string)                     {}

// SessionPersister stores a session durably. It is called before and after
// every refresh, because refresh tokens are single use.
type SessionPersister func(ctx context.Context, sess Session) error

// SessionTokenSource refreshes a bearer session on demand. All refreshes are
// serialised so that a refresh token is never presented twice.
type SessionTokenSource struct {
	client  *Client
	persist SessionPersister
	// Skew is how long before expiry a token counts as expired.
	Skew time.Duration
	// OnPersistError is called when a rotated session could not be stored.
	OnPersistError func(error)

	mu   sync.Mutex
	sess Session
}

// NewSessionTokenSource returns a TokenSource for sess. The client is only used
// for its refresh endpoint and may be the same client that uses the source.
func NewSessionTokenSource(client *Client, sess Session, persist SessionPersister) *SessionTokenSource {
	return &SessionTokenSource{client: client, sess: sess, persist: persist, Skew: time.Minute}
}

// Session returns a copy of the current session.
func (s *SessionTokenSource) Session() Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess
}

func (s *SessionTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess.AccessToken != "" && s.sess.PendingRefreshID == "" && s.client.now().Add(s.Skew).Before(s.sess.AccessExpiry) {
		return s.sess.AccessToken, nil
	}
	if err := s.refreshLocked(ctx); err != nil {
		return "", err
	}
	return s.sess.AccessToken, nil
}

func (s *SessionTokenSource) Invalidate(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == s.sess.AccessToken {
		s.sess.AccessExpiry = time.Time{}
	}
}

func (s *SessionTokenSource) refreshLocked(ctx context.Context) error {
	if s.sess.RefreshToken == "" {
		return ErrSessionRevoked
	}
	if !s.sess.RefreshExpiry.IsZero() && s.client.now().After(s.sess.RefreshExpiry) {
		return ErrSessionRevoked
	}
	pending := PrepareRefresh(s.sess)
	if pending.PendingRefreshID != s.sess.PendingRefreshID && s.persist != nil {
		if err := s.persist(ctx, pending); err != nil {
			return err
		}
	}
	s.sess = pending
	next, err := s.client.RefreshSession(ctx, pending)
	if err != nil {
		result := "error"
		if errors.Is(err, ErrSessionRevoked) {
			result = "revoked"
		}
		bridgemetrics.TokenRefreshes.WithLabelValues(result).Inc()
		return err
	}
	bridgemetrics.TokenRefreshes.WithLabelValues("ok").Inc()
	s.sess = *next
	if s.persist != nil {
		// The new refresh token is already live; failing to persist it means
		// a restart will present the old one and lose the session. Keep going
		// with the new tokens in memory and report the error.
		if err := s.persist(ctx, *next); err != nil && s.OnPersistError != nil {
			s.OnPersistError(err)
		}
	}
	return nil
}
