package connector

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
	"github.com/elsbrock/mautrix-chatto/pkg/chattotest"
)

// otherServer is the ID of the second fake server in multi-server tests.
const otherServer = "other"

func withGroupSpaces() harnessOpt {
	return func(hc *harnessConfig) { hc.network.GroupSpaces = true }
}

// serverSpaceKey returns the portal key of a server's space.
func serverSpaceKey(serverID string) networkid.PortalKey {
	return networkid.PortalKey{ID: chattoid.MakeServerPortalID(serverID)}
}

// waitSpaceChild waits until child is listed in the space room.
func (h *harness) waitSpaceChild(space, child id.RoomID) {
	h.t.Helper()
	h.eventually("space child "+string(child), func() bool {
		return len(h.mx.Events(func(e *sentEvent) bool {
			return e.Type == event.StateSpaceChild && e.RoomID == space && e.StateKey != nil && *e.StateKey == string(child)
		})) > 0
	})
}

func TestChannelsAndDMsSitInServerSpace(t *testing.T) {
	h := newHarness(t)
	h.srv.Name = "Acme Chat"
	alice := h.srv.AddUser("alice", "Alice", "pw")
	bob := h.srv.AddUser("bob", "Bob", "pw")
	group := h.srv.AddGroup("Engineering")
	grouped := h.srv.AddChannel("backend", group.ID, alice.ID)
	loose := h.srv.AddChannel("general", "", alice.ID)
	dm := h.srv.AddDM(alice.ID, bob.ID)
	h.login(aliceMXID, "alice", "pw")

	space := h.portalByID(serverSpaceKey(testServer))
	h.eventually("server space metadata", func() bool {
		p, _ := h.br.DB.Portal.GetByKey(h.ctx, serverSpaceKey(testServer))
		return p.RoomType == database.RoomTypeSpace && p.Name == "Acme Chat"
	})
	for _, roomID := range []string{grouped.ID, loose.ID, dm.ID} {
		h.waitSpaceChild(space.MXID, h.portal(roomID).MXID)
	}
	// Without group_spaces, room groups get no portal at all.
	if p, _ := h.br.DB.Portal.GetByKey(h.ctx, chattoidGroupKey(group.ID)); p != nil {
		t.Fatalf("group portal %s exists without group_spaces", p.ID)
	}
}

func TestRoomGroupBecomesSubspace(t *testing.T) {
	h := newHarness(t, withGroupSpaces())
	alice := h.srv.AddUser("alice", "Alice", "pw")
	group := h.srv.AddGroup("Engineering")
	grouped := h.srv.AddChannel("backend", group.ID, alice.ID)
	loose := h.srv.AddChannel("general", "", alice.ID)
	h.login(aliceMXID, "alice", "pw")

	server := h.portalByID(serverSpaceKey(testServer))
	space := h.portalByID(chattoidGroupKey(group.ID))
	h.eventually("group space metadata", func() bool {
		p, _ := h.br.DB.Portal.GetByKey(h.ctx, chattoidGroupKey(group.ID))
		return p.RoomType == database.RoomTypeSpace && p.Name == "Engineering"
	})
	h.waitSpaceChild(space.MXID, h.portal(grouped.ID).MXID)
	h.waitSpaceChild(server.MXID, space.MXID)
	h.waitSpaceChild(server.MXID, h.portal(loose.ID).MXID)
}

// twoServerWorld has alice logged into two servers whose fakes hand out the
// same IDs, so any ID that lacks its server prefix would collide.
type twoServerWorld struct {
	*harness
	main, other *chattotest.Server
	// alice, bob and the rooms have the same IDs on both servers.
	alice, bob                 *chattotest.User
	mainRoom, otherRoom        *chattotest.Room
	mainLogin, otherLoginAlice *bridgev2.UserLogin
}

func newTwoServerWorld(t *testing.T) *twoServerWorld {
	h := newHarness(t, withServer(otherServer))
	w := &twoServerWorld{harness: h, main: h.srv, other: h.srvs[otherServer]}
	var otherAlice *chattotest.User
	w.alice = w.main.AddUser("alice", "Alice", "pw")
	otherAlice = w.other.AddUser("alice", "Alice Elsewhere", "pw")
	w.bob = w.main.AddUser("bob", "Bob", "pw")
	w.other.AddUser("bob", "Bob Elsewhere", "pw")
	w.mainRoom = w.main.AddChannel("general", "", w.alice.ID, w.bob.ID)
	w.otherRoom = w.other.AddChannel("general", "", otherAlice.ID, w.bob.ID)
	if otherAlice.ID != w.alice.ID || w.otherRoom.ID != w.mainRoom.ID {
		t.Fatal("fake servers no longer hand out identical IDs; this test needs them to")
	}
	w.mainLogin = h.loginOn(testServer, aliceMXID, "alice", "pw")
	w.otherLoginAlice = h.loginOn(otherServer, aliceMXID, "alice", "pw")
	return w
}

func TestSameChattoIDsOnTwoServersStaySeparate(t *testing.T) {
	w := newTwoServerWorld(t)
	if w.mainLogin.ID == w.otherLoginAlice.ID {
		t.Fatalf("both logins got ID %s", w.mainLogin.ID)
	}
	if !strings.HasSuffix(w.otherLoginAlice.RemoteName, " on "+otherServer) {
		t.Errorf("remote name %q does not name the server", w.otherLoginAlice.RemoteName)
	}
	// The second login must not have replaced the first.
	if w.br.GetCachedUserLoginByID(w.mainLogin.ID) == nil {
		t.Fatal("main login was deleted by the other server's login")
	}

	mainPortal := w.portalByID(chattoid.MakePortalKey(testServer, w.mainRoom.ID))
	otherPortal := w.portalByID(chattoid.MakePortalKey(otherServer, w.otherRoom.ID))
	if mainPortal.MXID == otherPortal.MXID {
		t.Fatal("both servers' rooms share one Matrix room")
	}

	// Chatto → Matrix: each server's message lands in its own room, from its
	// own ghost.
	mainMsg := w.main.PostMessage(w.bob.ID, w.mainRoom.ID, "hello from main")
	otherMsg := w.other.PostMessage(w.bob.ID, w.otherRoom.ID, "hello from other")
	if mainMsg.ID != otherMsg.ID {
		t.Fatal("fake servers no longer hand out identical message IDs")
	}
	mainEvt := w.waitMessage(mainPortal.MXID, "hello from main")
	otherEvt := w.waitMessage(otherPortal.MXID, "hello from other")
	if mainEvt.Sender == otherEvt.Sender {
		t.Errorf("bob on both servers is the same ghost %s", mainEvt.Sender)
	}

	// Matrix → Chatto: a message in the other server's room only reaches it.
	w.sendMatrix(otherPortal.MXID, aliceMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "reply to other"})
	w.eventually("reply on other server", func() bool {
		for _, m := range w.other.Messages(w.otherRoom.ID) {
			if m.Body == "reply to other" {
				return true
			}
		}
		return false
	})
	for _, m := range w.main.Messages(w.mainRoom.ID) {
		if m.Body == "reply to other" {
			t.Fatal("reply leaked to the main server")
		}
	}
}

func TestRelayFromOtherServerIsRefused(t *testing.T) {
	w := newTwoServerWorld(t)
	dbPortal := w.portalByID(chattoid.MakePortalKey(testServer, w.mainRoom.ID))
	portal, err := w.br.GetPortalByKey(w.ctx, chattoid.MakePortalKey(testServer, w.mainRoom.ID))
	if err != nil {
		t.Fatal(err)
	}
	// bridgev2 lets set-relay pick any of the user's logins.
	if err := portal.SetRelay(w.ctx, w.otherLoginAlice); err != nil {
		t.Fatal(err)
	}
	w.mx.mu.Lock()
	w.mx.rooms[dbPortal.MXID].members[carolMXID] = event.MembershipJoin
	w.mx.mu.Unlock()
	w.sendMatrix(dbPortal.MXID, carolMXID, event.EventMessage, &event.MessageEventContent{MsgType: event.MsgText, Body: "crossed wires"})

	cc := w.otherLoginAlice.Client.(*ChattoClient)
	_, err = cc.HandleMatrixMessage(w.ctx, &bridgev2.MatrixMessage{MatrixEventBase: bridgev2.MatrixEventBase[*event.MessageEventContent]{
		Event:   &event.Event{ID: "$direct:test", Sender: carolMXID},
		Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "crossed wires"},
		Portal:  portal,
	}})
	if err == nil || !strings.Contains(err.Error(), errServerMismatch.Error()) {
		t.Fatalf("expected a server mismatch error, got %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	for _, srv := range []*chattotest.Server{w.main, w.other} {
		for _, m := range srv.Messages(w.mainRoom.ID) {
			if strings.Contains(m.Body, "crossed wires") {
				t.Fatalf("relayed message reached a server: %q", m.Body)
			}
		}
	}
}

func TestLoginAsksForServer(t *testing.T) {
	h := newHarness(t, withServer(otherServer))
	user, _ := h.br.GetUserByMXID(h.ctx, aliceMXID)
	proc, err := h.conn.CreateLogin(h.ctx, user, LoginFlowPassword)
	if err != nil {
		t.Fatal(err)
	}
	step, err := proc.Start(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	fields := step.UserInputParams.Fields
	if len(fields) != 3 || fields[0].ID != serverFieldID || fields[0].Type != bridgev2.LoginInputFieldTypeSelect ||
		strings.Join(fields[0].Options, ",") != testServer+","+otherServer {
		t.Fatalf("fields %+v", fields)
	}
	_, err = proc.(bridgev2.LoginProcessUserInput).SubmitUserInput(h.ctx, map[string]string{serverFieldID: "nope", "login": "alice", "password": "pw"})
	if !isRespError(err, ErrUnknownServer) {
		t.Fatalf("unknown server: %v", err)
	}
}

func TestSingleServerLoginHasNoServerField(t *testing.T) {
	h := newHarness(t)
	user, _ := h.br.GetUserByMXID(h.ctx, aliceMXID)
	proc, _ := h.conn.CreateLogin(h.ctx, user, LoginFlowToken)
	step, err := proc.Start(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range step.UserInputParams.Fields {
		if f.ID == serverFieldID {
			t.Fatal("server field shown with one server configured")
		}
	}
}

func TestOAuthLoginUsesChosenServer(t *testing.T) {
	h := newHarness(t, withPublicRouter(), withServer(otherServer))
	user, _ := h.br.GetUserByMXID(h.ctx, aliceMXID)
	proc, err := h.conn.CreateLogin(h.ctx, user, LoginFlowOAuth)
	if err != nil {
		t.Fatal(err)
	}
	step, err := proc.Start(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if step.Type != bridgev2.LoginStepTypeUserInput || step.UserInputParams.Fields[0].ID != serverFieldID {
		t.Fatalf("first step %+v", step)
	}
	step, err = proc.(bridgev2.LoginProcessUserInput).SubmitUserInput(h.ctx, map[string]string{serverFieldID: otherServer})
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Cancel()
	if want := h.srvs[otherServer].URL() + "/oauth/authorize?"; !strings.HasPrefix(step.DisplayAndWaitParams.Data, want) {
		t.Fatalf("authorize URL %q is not on %s", step.DisplayAndWaitParams.Data, want)
	}
}

func TestLoginOfUnconfiguredServerStaysLoggedOutable(t *testing.T) {
	h := newHarness(t)
	user, err := h.br.GetUserByMXID(h.ctx, aliceMXID)
	if err != nil {
		t.Fatal(err)
	}
	loginID := chattoid.MakeUserLoginID("gone", "U1")
	err = h.br.DB.UserLogin.Insert(h.ctx, &database.UserLogin{
		ID:         loginID,
		UserMXID:   user.MXID,
		RemoteName: "alice",
		Metadata:   &chattoid.UserLoginMetadata{Kind: chattoid.CredentialSession, Server: "gone"},
	})
	if err != nil {
		t.Fatal(err)
	}
	login, err := h.br.GetExistingUserLoginByID(h.ctx, loginID)
	if err != nil || login == nil {
		t.Fatalf("login not loaded: %v", err)
	}
	login.Client.Connect(h.ctx)
	if state := h.conn.loginState(loginID); state != status.StateUnknownError {
		t.Fatalf("state %q", state)
	}
	login.Logout(h.ctx)
	if h.br.GetCachedUserLoginByID(loginID) != nil {
		t.Fatal("login survived logout")
	}
}
