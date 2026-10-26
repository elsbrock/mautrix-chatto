package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
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
	roomID, groupID := chattoid.ParsePortalID(portal.ID)
	if groupID != "" {
		return c.getGroupInfo(ctx, groupID)
	}
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
		if len(members) <= 2 {
			info.Type = ptr.Ptr(database.RoomTypeDM)
			for _, uid := range members {
				if uid != c.userID {
					info.Members.OtherUserID = chattoid.MakeUserID(uid)
				}
			}
		} else {
			info.Type = ptr.Ptr(database.RoomTypeGroupDM)
		}
	default:
		info.Type = ptr.Ptr(database.RoomTypeDefault)
		info.Name = ptr.Ptr(room.GetName())
		info.Topic = ptr.Ptr(channelTopic(room))
		if room.GetGroupId() != "" {
			info.ParentID = ptr.Ptr(chattoid.MakeGroupPortalID(room.GetGroupId()))
		} else {
			info.ParentID = ptr.Ptr(networkid.PortalID(""))
		}
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
					if c.Main.IsGhostBot(ctx, uid) {
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
		Sender:   chattoid.MakeUserID(userID),
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

// getGroupInfo builds the space portal of a Chatto room group.
func (c *ChattoClient) getGroupInfo(ctx context.Context, groupID string) (*bridgev2.ChatInfo, error) {
	resp, err := c.api.RoomDirectory.GetRoomGroup(ctx, connect.NewRequest(&apiv1.GetRoomGroupRequest{GroupId: groupID}))
	if err != nil {
		return nil, fmt.Errorf("failed to get room group: %w", err)
	}
	group := resp.Msg.GetGroup()
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
	}, nil
}

func (c *ChattoClient) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	user, err := c.users.Get(ctx, chattoid.ParseUserID(ghost.ID))
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
