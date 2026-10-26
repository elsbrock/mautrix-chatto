package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattoid"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
)

var ErrBotsCannotStartDMs = bridgev2.RespError{
	ErrCode:    "RUN.CHATTO.BOT_CANNOT_START_DM",
	Err:        "Chatto bot accounts cannot start direct messages; a human has to start the DM with the bot",
	StatusCode: 403,
}

// lookupUser resolves "@login", "login" or a user ID.
func (c *ChattoClient) lookupUser(ctx context.Context, identifier string) (*apiv1.User, error) {
	identifier = strings.TrimSpace(identifier)
	identifier = strings.TrimPrefix(identifier, "@")
	if identifier == "" {
		return nil, errors.New("empty identifier")
	}
	if validChattoID(identifier, 'U') {
		if user, err := c.users.Get(ctx, identifier); err == nil {
			return user, nil
		}
	}
	if user, ok := c.users.GetByLogin(ctx, identifier); ok {
		return user, nil
	}
	return nil, nil
}

func (c *ChattoClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	user, err := c.lookupUser(ctx, identifier)
	if err != nil {
		return nil, err
	} else if user == nil {
		return nil, nil
	}
	resp, err := c.userToResolveResponse(ctx, user)
	if err != nil {
		return nil, err
	}
	if createChat {
		resp.Chat, err = c.startDM(ctx, user.GetId())
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (c *ChattoClient) userToResolveResponse(ctx context.Context, user *apiv1.User) (*bridgev2.ResolveIdentifierResponse, error) {
	userID := chattoid.MakeUserID(user.GetId())
	ghost, err := c.Main.Bridge.GetGhostByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get ghost: %w", err)
	}
	return &bridgev2.ResolveIdentifierResponse{
		Ghost:    ghost,
		UserID:   userID,
		UserInfo: c.userToUserInfo(user),
	}, nil
}

func (c *ChattoClient) startDM(ctx context.Context, userID string) (*bridgev2.CreateChatResponse, error) {
	if c.meta().IsBot {
		return nil, ErrBotsCannotStartDMs
	}
	resp, err := c.api.Rooms.StartDM(ctx, connect.NewRequest(&apiv1.StartDMRequest{ParticipantIds: []string{userID}}))
	if err != nil {
		return nil, fmt.Errorf("failed to start DM: %w", wrapSendError(err))
	}
	roomID := resp.Msg.GetRoom().GetId()
	full, err := c.api.RoomDirectory.GetRoom(ctx, connect.NewRequest(&apiv1.GetRoomRequest{RoomId: roomID}))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch DM: %w", err)
	}
	info, err := c.roomToChatInfo(ctx, full.Msg.GetRoom(), false)
	if err != nil {
		return nil, err
	}
	return &bridgev2.CreateChatResponse{PortalKey: chattoid.MakePortalKey(roomID), PortalInfo: info}, nil
}

func (c *ChattoClient) CreateChatWithGhost(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.CreateChatResponse, error) {
	return c.startDM(ctx, chattoid.ParseUserID(ghost.ID))
}

func (c *ChattoClient) GetContactList(ctx context.Context) ([]*bridgev2.ResolveIdentifierResponse, error) {
	return c.listUsers(ctx, "", 500)
}

func (c *ChattoClient) SearchUsers(ctx context.Context, query string) ([]*bridgev2.ResolveIdentifierResponse, error) {
	return c.listUsers(ctx, query, 50)
}

func (c *ChattoClient) listUsers(ctx context.Context, search string, limit int32) ([]*bridgev2.ResolveIdentifierResponse, error) {
	resp, err := c.api.Users.ListUsers(ctx, connect.NewRequest(&apiv1.ListUsersRequest{
		Search: search,
		Page:   &apiv1.PageRequest{Limit: limit},
	}))
	if err != nil {
		return nil, err
	}
	out := make([]*bridgev2.ResolveIdentifierResponse, 0, len(resp.Msg.GetUsers()))
	for _, member := range resp.Msg.GetUsers() {
		user := member.GetUser()
		if user.GetDeleted() || user.GetId() == c.userID || c.Main.IsGhostBot(ctx, user.GetId()) {
			continue
		}
		c.users.Put(user)
		r, err := c.userToResolveResponse(ctx, user)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
