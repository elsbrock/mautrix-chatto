package connector

import (
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
		Args: "<_bot login or ID_> [server|room <_room ID_>|group <_group ID_>] [--revoke]",
	},
	RequiresLogin: true,
}

var cmdAcceptInvite = &commands.FullHandler{
	Func: fnAcceptInvite,
	Name: "accept-invite",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionChats,
		Description: "Accept a Chatto room invitation",
		Args:        "<_invitation ID_>",
	},
	RequiresLogin: true,
}

var cmdServerInfo = &commands.FullHandler{
	Func: fnServerInfo,
	Name: "chatto-server",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Show the Chatto server this bridge talks to and its version",
	},
}

func humanLogin(ce *commands.Event) (*bridgev2.UserLogin, *ChattoClient) {
	for _, login := range ce.User.GetUserLogins() {
		cc, ok := login.Client.(*ChattoClient)
		if ok && cc.api != nil && !cc.isBot() {
			return login, cc
		}
	}
	return nil, nil
}

func fnGrantBotPermissions(ce *commands.Event) {
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `grant-bot-permissions <bot login or ID> [server|room <room ID>|group <group ID>] [--revoke]`")
		return
	}
	_, cc := humanLogin(ce)
	if cc == nil {
		ce.Reply("You need to be logged in with a human Chatto account that owns the bot.")
		return
	}
	args := ce.Args
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
		if roomID, _ := chattoid.ParsePortalID(ce.Portal.ID); roomID != "" {
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
	if len(ce.Args) < 1 {
		ce.Reply("Usage: `accept-invite <invitation ID>`")
		return
	}
	_, cc := humanLogin(ce)
	if cc == nil {
		ce.Reply("Invitations can only be accepted by human Chatto accounts.")
		return
	}
	_, err := cc.api.RoomInvitations.AcceptRoomInvitation(ce.Ctx, connect.NewRequest(&apiv1.AcceptRoomInvitationRequest{InvitationId: ce.Args[0]}))
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
	version := c.serverVersion
	if version == "" {
		version = "unknown"
	}
	ce.Reply("Chatto server: %s\nServer version: %s\nBridge API version: %s",
		c.Config.ServerURL, version, chattoclient.UpstreamVersion)
}
