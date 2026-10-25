package chattoclient_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattotest"
)

func TestParseServerURL(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "chat.example.com", want: "https://chat.example.com"},
		{in: "https://chat.example.com/some/path?x=1", want: "https://chat.example.com"},
		{in: "http://localhost:4000", want: "http://localhost:4000"},
		{in: "  https://chat.example.com  ", want: "https://chat.example.com"},
		{in: "", wantErr: true},
		{in: "ftp://chat.example.com", wantErr: true},
		{in: "https://", wantErr: true},
	}
	for _, tt := range tests {
		got, err := chattoclient.ParseServerURL(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseServerURL(%q) = %v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got.String() != tt.want {
			t.Errorf("ParseServerURL(%q) = %v, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestResolveURL(t *testing.T) {
	c, err := chattoclient.New("https://chat.example.com", chattoclient.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ResolveURL("/assets/files/A1?access=x"); got != "https://chat.example.com/assets/files/A1?access=x" {
		t.Errorf("relative: %q", got)
	}
	if got := c.ResolveURL("https://s3.example.org/a"); got != "https://s3.example.org/a" {
		t.Errorf("absolute: %q", got)
	}
}

func TestPasswordLogin(t *testing.T) {
	srv := chattotest.New()
	defer srv.Close()
	alice := srv.AddUser("alice", "Alice", "hunter2")
	c, _ := chattoclient.New(srv.URL(), chattoclient.Options{})

	res, err := c.PasswordLogin(context.Background(), "alice", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if res.UserID != alice.ID || res.Login != "alice" || res.Session.AccessToken == "" || res.Session.RefreshToken == "" {
		t.Fatalf("unexpected result %+v", res)
	}
	if time.Until(res.Session.AccessExpiry) < 10*time.Minute {
		t.Fatalf("access expiry too soon: %v", res.Session.AccessExpiry)
	}

	if _, err := c.PasswordLogin(context.Background(), "alice", "wrong"); !errors.Is(err, chattoclient.ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	srv.DirectLogin = false
	if _, err := c.PasswordLogin(context.Background(), "alice", "hunter2"); !errors.Is(err, chattoclient.ErrDirectLoginOff) {
		t.Fatalf("disabled login: %v", err)
	}
}

func TestRefreshRotationAndReuse(t *testing.T) {
	srv := chattotest.New()
	defer srv.Close()
	alice := srv.AddUser("alice", "Alice", "pw")
	_, refresh := srv.IssueSession(alice.ID)
	c, _ := chattoclient.New(srv.URL(), chattoclient.Options{})

	first, err := c.RefreshSession(context.Background(), chattoclient.Session{RefreshToken: refresh})
	if err != nil {
		t.Fatal(err)
	}
	if first.RefreshToken == refresh {
		t.Fatal("refresh token was not rotated")
	}
	// Presenting the rotated-away token revokes the session.
	if _, err := c.RefreshSession(context.Background(), chattoclient.Session{RefreshToken: refresh}); !errors.Is(err, chattoclient.ErrSessionRevoked) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := c.RefreshSession(context.Background(), *first); !errors.Is(err, chattoclient.ErrSessionRevoked) {
		t.Fatalf("after reuse the whole session must be gone: %v", err)
	}
}

func TestSessionTokenSourcePersistsAroundRefresh(t *testing.T) {
	srv := chattotest.New()
	defer srv.Close()
	alice := srv.AddUser("alice", "Alice", "pw")
	access, refresh := srv.IssueSession(alice.ID)
	base, _ := chattoclient.New(srv.URL(), chattoclient.Options{})

	var mu sync.Mutex
	var persisted []chattoclient.Session
	src := chattoclient.NewSessionTokenSource(base, chattoclient.Session{
		AccessToken: access, RefreshToken: refresh, AccessExpiry: time.Now().Add(-time.Minute),
	}, func(_ context.Context, s chattoclient.Session) error {
		mu.Lock()
		persisted = append(persisted, s)
		mu.Unlock()
		return nil
	})
	// Concurrent callers must cause exactly one refresh.
	var wg sync.WaitGroup
	tokens := make([]string, 8)
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := src.Token(context.Background())
			if err != nil {
				t.Error(err)
			}
			tokens[i] = tok
		}()
	}
	wg.Wait()
	for _, tok := range tokens {
		if tok != tokens[0] || tok == access {
			t.Fatalf("tokens differ or were not refreshed: %v", tokens)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(persisted) != 2 {
		t.Fatalf("expected persist before and after refresh, got %d", len(persisted))
	}
	if persisted[0].PendingRefreshID == "" || persisted[0].RefreshToken != refresh {
		t.Fatalf("first persist must record the pending refresh: %+v", persisted[0])
	}
	if persisted[1].PendingRefreshID != "" || persisted[1].RefreshToken == refresh {
		t.Fatalf("second persist must hold the new tokens: %+v", persisted[1])
	}
}

func TestInterceptorRefreshesOnUnauthenticated(t *testing.T) {
	srv := chattotest.New()
	defer srv.Close()
	alice := srv.AddUser("alice", "Alice", "pw")
	access, refresh := srv.IssueSession(alice.ID)
	base, _ := chattoclient.New(srv.URL(), chattoclient.Options{})
	src := chattoclient.NewSessionTokenSource(base, chattoclient.Session{
		AccessToken: access, RefreshToken: refresh, AccessExpiry: time.Now().Add(time.Hour),
	}, nil)
	c := base.WithTokens(src)
	srv.ExpireAccessTokens()
	resp, err := c.Viewer.GetViewer(context.Background(), connect.NewRequest(&apiv1.GetViewerRequest{}))
	if err != nil {
		t.Fatalf("GetViewer after expiry: %v", err)
	}
	if resp.Msg.GetUser().GetProfile().GetId() != alice.ID {
		t.Fatalf("wrong viewer")
	}
	if src.Session().AccessToken == access {
		t.Fatal("token was not refreshed")
	}
}

func TestStaticTokenRejected(t *testing.T) {
	srv := chattotest.New()
	defer srv.Close()
	c, _ := chattoclient.New(srv.URL(), chattoclient.Options{Tokens: chattoclient.StaticToken("cht_BK_nope")})
	_, err := c.Viewer.GetViewer(context.Background(), connect.NewRequest(&apiv1.GetViewerRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}
}

func TestServerVersion(t *testing.T) {
	srv := chattotest.New()
	defer srv.Close()
	c, _ := chattoclient.New(srv.URL(), chattoclient.Options{})
	v, err := c.ServerVersion(context.Background())
	if err != nil || v != chattotest.Version {
		t.Fatalf("ServerVersion = %q, %v", v, err)
	}
}

func TestLogout(t *testing.T) {
	srv := chattotest.New()
	defer srv.Close()
	alice := srv.AddUser("alice", "Alice", "pw")
	access, refresh := srv.IssueSession(alice.ID)
	c, _ := chattoclient.New(srv.URL(), chattoclient.Options{})
	if err := c.Logout(context.Background(), chattoclient.Session{AccessToken: access, RefreshToken: refresh}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshSession(context.Background(), chattoclient.Session{RefreshToken: refresh}); !errors.Is(err, chattoclient.ErrSessionRevoked) {
		t.Fatalf("refresh after logout: %v", err)
	}
}

func TestIsBotAPIKey(t *testing.T) {
	if !chattoclient.IsBotAPIKey("cht_BK_abc") || chattoclient.IsBotAPIKey("cht_RT_abc") {
		t.Fatal("IsBotAPIKey misclassifies")
	}
}
