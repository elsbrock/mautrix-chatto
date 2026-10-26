package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/elsbrock/mautrix-chatto/pkg/chattoid"
	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
)

const (
	portalKindChannel = "channel"
	portalKindDM      = "dm"
	portalKindGroupDM = "group_dm"
	portalKindSpace   = "space"

	// maxMemberSync caps how many channel members are synced to Matrix.
	maxMemberSync = 2000
)

func (c *ChattoClient) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	srv, kind, chattoID := chattoid.ParsePortalID(portal.ID)
	if srv != c.serverID {
		return nil, ErrServerMismatch
	}
	switch kind {
	case chattoid.PortalGroup:
		return c.getGroupInfo(ctx, chattoID)
	case chattoid.PortalRoom:
	default:
		return nil, fmt.Errorf("invalid portal ID %q", portal.ID)
	}
	roomID := chattoID
	resp, err := c.api.RoomDirectory.GetRoom(ctx, connect.NewRequest(&apiv1.GetRoomRequest{RoomId: roomID}))
	if err != nil {
		return nil, fmt.Errorf("failed to get room: %w", err)
	}
	return c.roomToChatInfo(ctx, resp.Msg.GetRoom(), true)
}

// roomToChatInfo converts a room. withMembers fetches the member list for
// channels, which costs extra requests.
func (c *ChattoClient) roomToChatInfo(ctx context.Context, rws *apiv1.RoomWithViewerState, withMembers bool) (*bridgev2.ChatInfo, error) {
	room := rws.GetRoom()
	info := &bridgev2.ChatInfo{
		CanBackfill: true,
		ExtraUpdates: func(_ context.Context, portal *bridgev2.Portal) bool {
			meta := portal.Metadata.(*chattoid.PortalMetadata)
			changed := meta.ThreadingMode != int32(room.GetThreadingMode()) ||
				meta.Universal != room.GetUniversal() || meta.Archived != room.GetArchived()
			meta.ThreadingMode = int32(room.GetThreadingMode())
			meta.Universal = room.GetUniversal()
			meta.Archived = room.GetArchived()
			kind := portalKindChannel
			if room.GetKind() == apiv1.RoomKind_ROOM_KIND_DM {
				kind = portalKindDM
				if len(rws.GetMemberUserIds()) > 2 {
					kind = portalKindGroupDM
				}
			}
			if meta.Kind != kind {
				meta.Kind = kind
				changed = true
			}
			return changed
		},
	}
	switch room.GetKind() {
	case apiv1.RoomKind_ROOM_KIND_DM:
		members := rws.GetMemberUserIds()
		info.Members = &bridgev2.ChatMemberList{
			IsFull:    true,
			MemberMap: make(bridgev2.ChatMemberMap, len(members)),
		}
		for _, uid := range members {
			info.Members.MemberMap.Set(c.makeMember(uid))
		}
		info.ParentID = ptr.Ptr(networkid.PortalID(""))
		if len(members) <= 2 {
			info.Type = ptr.Ptr(database.RoomTypeDM)
			for _, uid := range members {
				if uid != c.userID {
					info.Members.OtherUserID = c.ghostID(uid)
				}
			}
		} else {
			info.Type = ptr.Ptr(database.RoomTypeGroupDM)
		}
	default:
		info.Type = ptr.Ptr(database.RoomTypeDefault)
		info.Name = ptr.Ptr(room.GetName())
		info.Topic = ptr.Ptr(channelTopic(room))
		info.ParentID = ptr.Ptr(c.channelParent(ctx, room))
		if withMembers {
			members, full, err := c.listMembers(ctx, room.GetId())
			if err != nil {
				zerolog.Ctx(ctx).Warn().Err(err).Str("room_id", room.GetId()).Msg("Failed to list room members")
			} else {
				info.Members = &bridgev2.ChatMemberList{
					IsFull:    full,
					MemberMap: make(bridgev2.ChatMemberMap, len(members)),
				}
				for _, uid := range members {
					if c.Main.IsGhostBot(ctx, c.ghostID(uid)) {
						continue
					}
					info.Members.MemberMap.Set(c.makeMember(uid))
				}
				// Our own login is always a member if it can see the room as one.
				if rws.GetViewerState().GetIsMember() {
					info.Members.MemberMap.Set(c.makeMember(c.userID))
				}
			}
		}
	}
	return info, nil
}

// channelTopic is the room description, annotated when the room is archived.
func channelTopic(room *apiv1.Room) string {
	topic := room.GetDescription()
	if room.GetArchived() {
		if topic == "" {
			return "[archived on Chatto]"
		}
		return "[archived on Chatto] " + topic
	}
	return topic
}

func (c *ChattoClient) makeMember(userID string) bridgev2.ChatMember {
	member := bridgev2.ChatMember{
		EventSender: c.makeEventSender(userID),
		Membership:  event.MembershipJoin,
	}
	return member
}

// makeEventSender builds the sender of an event by a Chatto user.
func (c *ChattoClient) makeEventSender(userID string) bridgev2.EventSender {
	sender := bridgev2.EventSender{
		IsFromMe: userID == c.userID,
		Sender:   c.ghostID(userID),
	}
	if sender.IsFromMe {
		sender.SenderLogin = c.UserLogin.ID
	}
	return sender
}

func (c *ChattoClient) listMembers(ctx context.Context, roomID string) ([]string, bool, error) {
	var all []string
	offset := int32(0)
	for {
		resp, err := c.api.Rooms.ListMembers(ctx, connect.NewRequest(&apiv1.ListMembersRequest{
			RoomId: roomID,
			Page:   &apiv1.PageRequest{Limit: 500, Offset: offset},
		}))
		if err != nil {
			return nil, false, err
		}
		ids := resp.Msg.GetUserIds()
		all = append(all, ids...)
		if !resp.Msg.GetPage().GetHasMore() || len(ids) == 0 {
			return all, true, nil
		}
		if len(all) >= maxMemberSync {
			return all, false, nil
		}
		offset += int32(len(ids))
	}
}

// channelParent returns the space a channel belongs to: its room group, or
// none for an ungrouped channel, which then sits in the personal space. The
// empty parent is explicit so a channel moved out of a group is re-parented.
func (c *ChattoClient) channelParent(ctx context.Context, room *apiv1.Room) networkid.PortalID {
	if room.GetGroupId() == "" {
		return ""
	}
	c.ensureGroupSpace(ctx, room.GetGroupId(), nil)
	return chattoid.MakeGroupPortalID(c.serverID, room.GetGroupId())
}

// ensureGroupSpace creates a room group's space before any of its channels
// and puts it in this login's personal space. With group set, an existing
// space is refreshed from it.
//
// bridgev2 creates a missing parent space from the child room's event
// context and abandons it when that context ends, which can leave the space
// half-created; creating it up front avoids that path.
func (c *ChattoClient) ensureGroupSpace(ctx context.Context, groupID string, group *apiv1.RoomGroup) {
	portal, err := c.Main.Bridge.GetPortalByKey(ctx, networkid.PortalKey{ID: chattoid.MakeGroupPortalID(c.serverID, groupID)})
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("group_id", groupID).Msg("Failed to get group space portal")
		return
	}
	switch {
	case portal.MXID != "" && group != nil:
		portal.UpdateInfo(ctx, c.groupToChatInfo(group), c.UserLogin, nil, time.Time{})
	case portal.MXID == "":
		var info *bridgev2.ChatInfo
		var err error
		if group != nil {
			info = c.groupToChatInfo(group)
		} else {
			info, err = c.getGroupInfo(ctx, groupID)
		}
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("group_id", groupID).Msg("Failed to get room group")
			return
		}
		if err := portal.CreateMatrixRoom(ctx, c.UserLogin, info); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("group_id", groupID).Msg("Failed to create group space")
			return
		}
	}
	c.UserLogin.MarkInPortal(ctx, portal)
}

// ensureGroupSpaces refreshes the spaces of all room groups that contain a
// room this login is a member of. Groups without such a room get no space.
func (c *ChattoClient) ensureGroupSpaces(ctx context.Context) {
	resp, err := c.api.RoomDirectory.ListRoomGroups(ctx, connect.NewRequest(&apiv1.ListRoomGroupsRequest{}))
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to list room groups")
		return
	}
	for _, group := range resp.Msg.GetGroups() {
		for _, item := range group.GetItems() {
			if item.GetRoom().GetViewerState().GetIsMember() {
				c.ensureGroupSpace(ctx, group.GetId(), group)
				break
			}
		}
	}
}

// syncPersonalSpace makes the login's personal filtering space the server's
// space: bridgev2 names it after the network and the login and gives it the
// network icon, and a login belongs to exactly one server. Without a
// profile the space is named after the server ID.
func (c *ChattoClient) syncPersonalSpace(ctx context.Context) {
	spaceRoom, err := c.UserLogin.GetSpaceRoom(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to get personal space")
		return
	} else if spaceRoom == "" {
		return
	}
	profile, err := c.api.ServerProfile(ctx)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to fetch Chatto server profile")
		return
	}
	name := profile.GetName()
	if name == "" {
		name = c.serverID
	}
	meta := c.UserLogin.Metadata.(*chattoid.UserLoginMetadata)
	sendState := func(evtType event.Type, content any) bool {
		_, err := c.Main.Bridge.Bot.SendState(ctx, spaceRoom, evtType, "", &event.Content{Parsed: content}, time.Time{})
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Stringer("event_type", evtType).Msg("Failed to update personal space")
		}
		return err == nil
	}
	changed := false
	if meta.SpaceName != name && sendState(event.StateRoomName, &event.RoomNameEventContent{Name: name}) {
		meta.SpaceName = name
		changed = true
	}
	if topic := profile.GetDescription(); meta.SpaceTopic != topic && sendState(event.StateTopic, &event.TopicEventContent{Topic: topic}) {
		meta.SpaceTopic = topic
		changed = true
	}
	if logo := c.api.ResolveURL(profile.GetLogoUrl()); meta.SpaceLogo != logo {
		var mxc id.ContentURIString
		if logo != "" {
			mxc, err = c.uploadPublic(ctx, spaceRoom, logo)
		}
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to upload server logo")
		} else if sendState(event.StateRoomAvatar, &event.RoomAvatarEventContent{URL: mxc}) {
			meta.SpaceLogo = logo
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := c.UserLogin.Save(ctx); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to save personal space state")
	}
}

// uploadPublic copies a public Chatto image to the Matrix media repo.
func (c *ChattoClient) uploadPublic(ctx context.Context, roomID id.RoomID, rawURL string) (id.ContentURIString, error) {
	data, err := c.downloadPublic(ctx, rawURL, 10<<20)
	if err != nil {
		return "", err
	}
	mxc, _, err := c.Main.Bridge.Bot.UploadMedia(ctx, roomID, data, "", http.DetectContentType(data))
	return mxc, err
}

// getGroupInfo builds the space portal of a Chatto room group.
func (c *ChattoClient) getGroupInfo(ctx context.Context, groupID string) (*bridgev2.ChatInfo, error) {
	resp, err := c.api.RoomDirectory.GetRoomGroup(ctx, connect.NewRequest(&apiv1.GetRoomGroupRequest{GroupId: groupID}))
	if err != nil {
		return nil, fmt.Errorf("failed to get room group: %w", err)
	}
	return c.groupToChatInfo(resp.Msg.GetGroup()), nil
}

// groupToChatInfo converts a room group. Groups have no parent, so bridgev2
// puts them in the personal space of every login that is in them.
func (c *ChattoClient) groupToChatInfo(group *apiv1.RoomGroup) *bridgev2.ChatInfo {
	return &bridgev2.ChatInfo{
		Name:  ptr.Ptr(group.GetName()),
		Topic: ptr.Ptr(group.GetDescription()),
		Type:  ptr.Ptr(database.RoomTypeSpace),
		Members: &bridgev2.ChatMemberList{
			IsFull:    false,
			MemberMap: bridgev2.ChatMemberMap{}.Set(c.makeMember(c.userID)),
		},
		ExtraUpdates: func(_ context.Context, portal *bridgev2.Portal) bool {
			meta := portal.Metadata.(*chattoid.PortalMetadata)
			if meta.Kind == portalKindSpace {
				return false
			}
			meta.Kind = portalKindSpace
			return true
		},
	}
}

func (c *ChattoClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	userID, ok := c.chattoUserID(ghost.ID)
	if !ok {
		return nil, ErrServerMismatch
	}
	user, err := c.users.Get(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	return c.userToUserInfo(user), nil
}

func (c *ChattoClient) userToUserInfo(user *apiv1.User) *bridgev2.UserInfo {
	isBot := user.GetBot() != nil
	name := c.Main.Config.FormatDisplayname(DisplaynameParams{
		DisplayName: user.GetDisplayName(),
		Login:       user.GetLogin(),
		ID:          user.GetId(),
		IsBot:       isBot,
	})
	if user.GetDeleted() {
		name = "Deleted user"
	}
	info := &bridgev2.UserInfo{
		Name:        &name,
		IsBot:       &isBot,
		Identifiers: []string{"chatto:" + user.GetLogin()},
		ExtraUpdates: func(_ context.Context, ghost *bridgev2.Ghost) bool {
			meta := ghost.Metadata.(*chattoid.GhostMetadata)
			if meta.Login == user.GetLogin() && meta.IsBot == isBot {
				return false
			}
			meta.Login = user.GetLogin()
			meta.IsBot = isBot
			return true
		},
	}
	if user.AvatarUrl != nil {
		avatarURL := c.api.ResolveURL(user.GetAvatarUrl())
		if avatarURL == "" {
			info.Avatar = &bridgev2.Avatar{Remove: true}
		} else {
			info.Avatar = &bridgev2.Avatar{
				ID: networkid.AvatarID(avatarURL),
				Get: func(ctx context.Context) ([]byte, error) {
					return c.downloadPublic(ctx, avatarURL, 10<<20)
				},
			}
		}
	} else {
		info.Avatar = &bridgev2.Avatar{Remove: true}
	}
	return info
}

// downloadPublic fetches a URL that needs no credentials (avatars, signed
// asset URLs), following redirects, with a size cap. Errors never include
// the query string, which carries Chatto access tickets.
func (c *ChattoClient) downloadPublic(ctx context.Context, rawURL string, maxSize int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid download URL %s", redactURL(rawURL))
	}
	req.Header.Set("User-Agent", c.Main.userAgent())
	resp, err := c.api.HTTP().Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, fmt.Errorf("downloading %s: %w", redactURL(rawURL), uerr.Err)
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP %d downloading %s", resp.StatusCode, redactURL(rawURL))
	}
	if maxSize > 0 && resp.ContentLength > maxSize {
		return nil, fmt.Errorf("file too large (%d bytes)", resp.ContentLength)
	}
	reader := io.Reader(resp.Body)
	if maxSize > 0 {
		reader = io.LimitReader(resp.Body, maxSize+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", redactURL(rawURL), err)
	}
	if maxSize > 0 && int64(len(data)) > maxSize {
		return nil, fmt.Errorf("file too large")
	}
	return data, nil
}

// redactURL strips query strings, which carry Chatto access tickets.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i] + "?…"
	}
	return u
}
