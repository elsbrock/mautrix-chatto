package connector

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
	"github.com/elsbrock/mautrix-chatto/pkg/chattotest"
)

func TestOAuthLogin(t *testing.T) {
	h := newHarness(t, withPublicRouter())
	alice := h.srv.AddUser("alice", "Alice", "pw")
	room := h.srv.AddChannel("general", "", alice.ID)

	found := false
	for _, f := range h.conn.GetLoginFlows() {
		found = found || f.ID == LoginFlowOAuth
	}
	if !found {
		t.Fatal("oauth flow not offered with a public address")
	}

	// The client metadata document must name itself as the client ID.
	resp, err := http.Get(h.conn.oauthClientID())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	_ = resp.Body.Close()
	if doc["client_id"] != h.conn.oauthClientID() || doc["token_endpoint_auth_method"] != "none" {
		t.Fatalf("metadata document %+v", doc)
	}
	// Chatto rejects documents whose client_uri is on another origin.
	if cu, _ := url.Parse(doc["client_uri"].(string)); cu.Scheme+"://"+cu.Host != h.conn.publicAddress() {
		t.Fatalf("client_uri %v is not on the client_id's origin", doc["client_uri"])
	}

	user, _ := h.br.GetUserByMXID(h.ctx, aliceMXID)
	proc, err := h.conn.CreateLogin(h.ctx, user, LoginFlowOAuth)
	if err != nil {
		t.Fatal(err)
	}
	step, err := proc.Start(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(step.DisplayAndWaitParams.Data)
	if err != nil {
		t.Fatal(err)
	}
	q := authURL.Query()
	if authURL.Path != "/oauth/authorize" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != h.conn.oauthClientID() {
		t.Fatalf("authorize URL %s", authURL)
	}
	// The user approves in the browser; Chatto redirects back with a code.
	code := h.srv.IssueAuthCode(chattotest.AuthCode{
		UserID: alice.ID, Challenge: q.Get("code_challenge"),
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"),
	})
	// The redirect shows a confirmation naming the Matrix account; linking
	// only happens on the POST.
	cb, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(cb.Body)
	_ = cb.Body.Close()
	if !strings.Contains(string(page), string(aliceMXID)) || !strings.Contains(string(page), `method="post"`) {
		t.Fatalf("confirmation page does not name the Matrix account: %s", page)
	}
	go func() {
		resp, err := http.PostForm(q.Get("redirect_uri"), url.Values{"code": {code}, "state": {q.Get("state")}})
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	defer cancel()
	done, err := proc.(bridgev2.LoginProcessDisplayAndWait).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	meta := done.CompleteParams.UserLogin.Metadata.(*chattoid.UserLoginMetadata)
	if meta.Session == nil || meta.Session.ClientID != h.conn.oauthClientID() {
		t.Fatalf("session %+v", meta.Session)
	}
	h.portal(room.ID)
}

func TestOAuthCallbackRejectsUnknownState(t *testing.T) {
	h := newHarness(t, withPublicRouter())
	resp, err := http.PostForm(h.conn.oauthRedirectURI(), url.Values{"code": {"x"}, "state": {"nope"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestOAuthNotOfferedWithoutPublicAddress(t *testing.T) {
	h := newHarness(t)
	for _, f := range h.conn.GetLoginFlows() {
		if f.ID == LoginFlowOAuth {
			t.Fatal("oauth offered without a public address")
		}
	}
}
