package connector

import (
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoclient"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	adminv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/admin/v1"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
)

// BotPermissions are the Chatto permissions a bot login needs to bridge a
// room fully. Bots get no baseline permissions on Chatto, and grants are
// capped by the owner's own permissions.
var BotPermissions = []string{
	"room.list",
	"room.join",
	"message.read",
	"message.post",
	"message.post-in-thread",
	"message.echo",
	"message.attach",
	"message.react",
}

var cmdGrantBotPermissions = &commands.FullHandler{
	Func: fnGrantBotPermissions,
	Name: "grant-bot-permissions",
	Help: commands.HelpMeta{
		Section: commands.HelpSectionAuth,
		Description: "Grant a Chatto bot account the permissions the bridge needs, using your (human) login. " +
			"Run inside a portal to scope the grant to that room and add the bot to it.",
		Args: "<_bot login or ID_> [server|room <_room ID_>|group <_group ID_>] [--server <_server_>] [--revoke]",
	},
	RequiresLogin: true,
}

var cmdAcceptInvite = &commands.FullHandler{
	Func: fnAcceptInvite,
	Name: "accept-invite",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Accept a Chatto room invitation",
		Args:        "<_invitation ID_> [--server <_server_>]",
	},
	RequiresLogin: true,
}

var cmdServerInfo = &commands.FullHandler{
	Func: fnServerInfo,
	Name: "chatto-server",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Show the Chatto servers this bridge talks to and their versions",
	},
}

// humanLogin returns the user's first connected human login on a server.
func humanLogin(ce *commands.Event, serverID string) (*bridgev2.UserLogin, *ChattoClient) {
	for _, login := range ce.User.GetUserLogins() {
		cc, ok := login.Client.(*ChattoClient)
		if ok && cc.api != nil && !cc.isBot() && cc.serverID == serverID {
			return login, cc
		}
	}
	return nil, nil
}

// commandServer picks the server a command acts on: an explicit --server,
// the portal's server, a server matched by match, or the only server the
// user is logged into. It returns the remaining arguments.
func commandServer(ce *commands.Event, args []string, match func(cc *ChattoClient) bool) (string, []string, error) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--server" {
			rest := append(append([]string(nil), args[:i]...), args[i+2:]...)
			return args[i+1], rest, nil
		}
	}
	if ce.Portal != nil {
		if srv, _, _ := chattoid.ParsePortalID(ce.Portal.ID); srv != "" {
			return srv, args, nil
		}
	}
	servers := map[string]bool{}
	var matched []string
	for _, login := range ce.User.GetUserLogins() {
		cc, ok := login.Client.(*ChattoClient)
		if !ok {
			continue
		}
		servers[cc.serverID] = true
		if match != nil && match(cc) {
			matched = append(matched, cc.serverID)
		}
	}
	if len(matched) == 1 {
		return matched[0], args, nil
	}
	if len(servers) == 1 {
		for srv := range servers {
			return srv, args, nil
		}
	}
	return "", args, errors.New("you are logged into several Chatto servers; add `--server <server>` to choose one")
}

func fnGrantBotPermissions(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `grant-bot-permissions <bot login or ID> [server|room <room ID>|group <group ID>] [--server <server>] [--revoke]`")
		return
	}
	botArg := ce.Args[0]
	serverID, args, err := commandServer(ce, ce.Args, func(cc *ChattoClient) bool {
		// The bot is usually a bridge login itself, which names its server.
		return cc.isBot() && (cc.userID == botArg || strings.EqualFold(cc.meta().Login, botArg))
	})
	if err != nil {
		ce.Reply("%v", err)
		return
	}
	_, cc := humanLogin(ce, serverID)
	if cc == nil {
		ce.Reply("You need to be logged in on %s with a human Chatto account that owns the bot.", serverID)
		return
	}
	revoke := false
	if args[len(args)-1] == "--revoke" {
		revoke = true
		args = args[:len(args)-1]
	}
	bot, err := cc.lookupUser(ce.Ctx, args[0])
	if err != nil || bot == nil {
		ce.Reply("Chatto user %q not found", args[0])
		return
	}
	if bot.GetBot() == nil {
		ce.Reply("%s is not a bot account. Human accounts get permissions through roles in Chatto's admin UI.", bot.GetLogin())
		return
	}
	scope := &adminv1.PermissionScope{Kind: adminv1.PermissionScopeKind_PERMISSION_SCOPE_KIND_SERVER}
	scopeName := "the whole server"
	roomToJoin := ""
	switch {
	case len(args) >= 3 && args[1] == "room":
		scope = &adminv1.PermissionScope{Kind: adminv1.PermissionScopeKind_PERMISSION_SCOPE_KIND_ROOM, Id: args[2]}
		scopeName, roomToJoin = "room "+args[2], args[2]
	case len(args) >= 3 && args[1] == "group":
		scope = &adminv1.PermissionScope{Kind: adminv1.PermissionScopeKind_PERMISSION_SCOPE_KIND_GROUP, Id: args[2]}
		scopeName = "room group " + args[2]
	case len(args) >= 2 && args[1] == "server":
	case ce.Portal != nil:
		if _, roomID := chattoid.ParseRoomPortalID(ce.Portal.ID); roomID != "" {
			scope = &adminv1.PermissionScope{Kind: adminv1.PermissionScopeKind_PERMISSION_SCOPE_KIND_ROOM, Id: roomID}
			scopeName, roomToJoin = "this room", roomID
		}
	}
	decision := adminv1.PermissionDecision_PERMISSION_DECISION_ALLOW
	if revoke {
		decision = adminv1.PermissionDecision_PERMISSION_DECISION_NONE
	}
	var failed []string
	for _, perm := range BotPermissions {
		_, err := cc.api.AdminPermissions.SetUserPermission(ce.Ctx, connect.NewRequest(&adminv1.SetUserPermissionRequest{
			UserId:     bot.GetId(),
			Permission: perm,
			Decision:   decision,
			Scope:      scope,
		}))
		if err != nil {
			failed = append(failed, fmt.Sprintf("`%s` (%s)", perm, connect.CodeOf(err)))
		}
	}
	verb := "Granted"
	if revoke {
		verb = "Revoked"
	}
	if len(failed) > 0 {
		ce.Reply("%s %d of %d permissions for %s in %s. Failed: %s.\n\nGrants are capped by your own permissions; "+
			"some permissions may also need privileged mode in Chatto's web UI.",
			verb, len(BotPermissions)-len(failed), len(BotPermissions), bot.GetLogin(), scopeName, strings.Join(failed, ", "))
	} else {
		ce.Reply("%s %s for %s in %s.", verb, "`"+strings.Join(BotPermissions, "`, `")+"`", bot.GetLogin(), scopeName)
	}
	if roomToJoin != "" && !revoke {
		_, err := cc.api.Rooms.AddMember(ce.Ctx, connect.NewRequest(&apiv1.AddMemberRequest{RoomId: roomToJoin, UserId: bot.GetId()}))
		if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
			ce.Reply("Could not add %s to the room: %v", bot.GetLogin(), err)
		} else if err == nil {
			ce.Reply("Added %s to the room.", bot.GetLogin())
		}
	}
}

func fnAcceptInvite(ce *commands.Event) {
	serverID, args, err := commandServer(ce, ce.Args, nil)
	if len(args) < 1 {
		ce.Reply("Usage: `accept-invite <invitation ID> [--server <server>]`")
		return
	} else if err != nil {
		ce.Reply("%v", err)
		return
	}
	_, cc := humanLogin(ce, serverID)
	if cc == nil {
		ce.Reply("Invitations can only be accepted by human Chatto accounts on %s.", serverID)
		return
	}
	_, err = cc.api.RoomInvitations.AcceptRoomInvitation(ce.Ctx, connect.NewRequest(&apiv1.AcceptRoomInvitationRequest{InvitationId: args[0]}))
	if err != nil {
		ce.Reply("Failed to accept invitation: %v", err)
		return
	}
	// The resulting user_joined_room event creates the portal. Remote events
	// are only queued from the realtime goroutine, because bridgev2 races
	// when two goroutines queue the first events of a new portal.
	ce.Reply("Invitation accepted.")
}

func fnServerInfo(ce *commands.Event) {
	c, ok := ce.Bridge.Network.(*Connector)
	if !ok {
		return
	}
	var sb strings.Builder
	for _, id := range c.serverIDs() {
		srv := c.server(id)
		version, tooOld := srv.serverVersion()
		if version == "" {
			version = "unknown"
		} else if tooOld {
			version += " (too old, refused)"
		}
		fmt.Fprintf(&sb, "* %s: %s, version %s\n", srv.ID, srv.URL, version)
	}
	ce.Reply("Chatto servers:\n%s\nBridge API version: %s", sb.String(), chattoclient.UpstreamVersion)
}
