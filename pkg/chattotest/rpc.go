package chattotest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adminv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/admin/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/admin/v1/adminv1connect"
	apiv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/api/v1/apiv1connect"
	discoveryv1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/discovery/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/discovery/v1/discoveryv1connect"
	realtimev1 "github.com/elsbrock/matrix-chatto-bridge/pkg/chattopb/chatto/realtime/v1"
	"github.com/elsbrock/matrix-chatto-bridge/pkg/emoji"
)

func errNotFound(what string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", what))
}

func errDenied(msg string) error {
	return connect.NewError(connect.CodePermissionDenied, errors.New(msg))
}

func errInvalid(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

// --- proto conversion ---

func (s *Server) userProtoLocked(u *User) *apiv1.User {
	p := &apiv1.User{Id: u.ID, Login: u.Login, DisplayName: u.DisplayName, PresenceStatus: u.Presence, CustomStatus: u.Status}
	if u.AvatarKey != "" {
		url := s.HTTP.URL + "/assets/server/" + u.AvatarKey
		p.AvatarUrl = &url
	}
	if u.BotOwner != "" {
		p.Bot = &apiv1.BotInfo{OwnerUserId: u.BotOwner}
	}
	return p
}

func (s *Server) sortedRoomsLocked() []*Room {
	rooms := make([]*Room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	slices.SortFunc(rooms, func(a, b *Room) int { return strings.Compare(a.ID, b.ID) })
	return rooms
}

func (s *Server) roomProto(r *Room) *apiv1.Room {
	return &apiv1.Room{
		Id: r.ID, Kind: r.Kind, Name: r.Name, Description: r.Description, Archived: r.Archived,
		GroupId: r.GroupID, Universal: r.Universal, ThreadingMode: r.ThreadingMode, CreatedAt: ts(r.CreatedAt),
	}
}

func (s *Server) roomWithViewerLocked(r *Room, userID string) *apiv1.RoomWithViewerState {
	rws := &apiv1.RoomWithViewerState{
		Room:        s.roomProto(r),
		ViewerState: &apiv1.RoomViewerState{IsMember: slices.Contains(r.Members, userID)},
	}
	if r.Kind == apiv1.RoomKind_ROOM_KIND_DM {
		rws.MemberUserIds = slices.Clone(r.Members)
	}
	return rws
}

func (s *Server) groupLocked(g *Group, userID string) *apiv1.RoomGroup {
	pg := &apiv1.RoomGroup{Id: g.ID, Name: g.Name, Description: g.Description}
	for _, rid := range g.RoomIDs {
		if r, ok := s.rooms[rid]; ok && !r.Archived {
			pg.Items = append(pg.Items, &apiv1.RoomGroupItem{Item: &apiv1.RoomGroupItem_Room{Room: s.roomWithViewerLocked(r, userID)}})
		}
	}
	return pg
}

func (s *Server) messageProtoLocked(m *Message, viewer string) *apiv1.Message {
	p := &apiv1.Message{
		Id: m.ID, RoomId: m.RoomID, CreatedAt: ts(m.CreatedAt), ActorId: m.ActorID,
		UpdatedAt: ts(m.UpdatedAt), InReplyTo: m.InReplyTo, ThreadRootEventId: m.ThreadRoot,
	}
	if !m.DeletedAt.IsZero() {
		p.DeletedAt = ts(m.DeletedAt)
		return p
	}
	body := m.Body
	p.Body = &body
	for _, a := range m.Attachments {
		pa := &apiv1.MessageAttachment{Id: a.ID, Filename: a.Filename, ContentType: a.ContentType}
		if a.Description != "" {
			d := a.Description
			pa.Description = &d
		}
		if a.Processing {
			pa.VideoProcessing = &apiv1.MessageVideoProcessing{Status: apiv1.MessageVideoProcessingStatus_MESSAGE_VIDEO_PROCESSING_STATUS_PROCESSING}
		} else {
			pa.AssetUrl = &apiv1.MessageAssetUrl{
				Url:       "/assets/files/" + a.ID + "?access=ticket-" + viewer,
				ExpiresAt: timestamppb.New(time.Now().Add(24 * time.Hour)),
			}
		}
		p.Attachments = append(p.Attachments, pa)
	}
	for _, e := range m.reactOrd {
		users := m.Reactions[e]
		if len(users) == 0 {
			continue
		}
		preview := users
		if len(preview) > 5 {
			preview = preview[:5]
		}
		p.Reactions = append(p.Reactions, &apiv1.MessageReaction{
			Emoji: e, Count: int32(len(users)), HasReacted: slices.Contains(users, viewer), PreviewUserIds: slices.Clone(preview),
		})
	}
	if replies := s.threads[m.ID]; len(replies) > 0 {
		p.Thread = &apiv1.ThreadSummary{ThreadRootEventId: m.ID, ReplyCount: int32(len(replies))}
	}
	return p
}

func (s *Server) timelineEventLocked(m *Message, viewer string) *apiv1.RoomTimelineEvent {
	return &apiv1.RoomTimelineEvent{
		Id: m.ID, CreatedAt: ts(m.CreatedAt), ActorId: m.ActorID,
		Event: &apiv1.RoomTimelineEvent_MessagePosted{MessagePosted: &apiv1.RoomMessagePosted{Message: s.messageProtoLocked(m, viewer)}},
	}
}

// page builds a timeline page over ids[start:end].
func (s *Server) pageLocked(ids []string, start, end int, viewer string, prefix []*Message) *apiv1.RoomTimelinePage {
	start, end = max(start, 0), min(end, len(ids))
	if start > end {
		start = end
	}
	page := &apiv1.RoomTimelinePage{
		StartCursor: "i:" + strconv.Itoa(start), EndCursor: "i:" + strconv.Itoa(end),
		HasOlder: start > 0, HasNewer: end < len(ids),
		Includes: &apiv1.RoomTimelineIncludes{Users: map[string]*apiv1.User{}},
	}
	add := func(m *Message) {
		page.Events = append(page.Events, s.timelineEventLocked(m, viewer))
		if u, ok := s.users[m.ActorID]; ok {
			page.Includes.Users[u.ID] = s.userProtoLocked(u)
		}
	}
	for _, m := range prefix {
		add(m)
	}
	for _, id := range ids[start:end] {
		add(s.messages[id])
	}
	return page
}

func parseCursor(c string) (int, bool) {
	v, ok := strings.CutPrefix(c, "i:")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	return n, err == nil
}

func limitOr(limit int32) int {
	if limit <= 0 {
		return 50
	}
	return int(min(limit, 500))
}

func (s *Server) memberRoomLocked(roomID, userID string) (*Room, error) {
	r, ok := s.rooms[roomID]
	if !ok {
		return nil, errNotFound("room")
	}
	if !slices.Contains(r.Members, userID) {
		return nil, errDenied("not a member")
	}
	return r, nil
}

// --- services ---

type discoveryService struct {
	discoveryv1connect.UnimplementedServerDiscoveryServiceHandler
	s *Server
}

func (d *discoveryService) GetServer(context.Context, *connect.Request[discoveryv1.GetServerRequest]) (*connect.Response[discoveryv1.GetServerResponse], error) {
	return connect.NewResponse(&discoveryv1.GetServerResponse{Profile: &apiv1.ServerPublicProfile{Version: Version, Name: "Fake Chatto"}}), nil
}

type viewerService struct {
	apiv1connect.UnimplementedViewerServiceHandler
	s *Server
}

func (v *viewerService) GetViewer(ctx context.Context, _ *connect.Request[apiv1.GetViewerRequest]) (*connect.Response[apiv1.GetViewerResponse], error) {
	v.s.mu.Lock()
	defer v.s.mu.Unlock()
	u := v.s.users[viewerID(ctx)]
	return connect.NewResponse(&apiv1.GetViewerResponse{User: &apiv1.ViewerUser{Profile: v.s.userProtoLocked(u)}}), nil
}

type accountService struct {
	apiv1connect.UnimplementedMyAccountServiceHandler
	s *Server
}

func (a *accountService) RefreshPresence(context.Context, *connect.Request[apiv1.RefreshPresenceRequest]) (*connect.Response[apiv1.RefreshPresenceResponse], error) {
	return connect.NewResponse(&apiv1.RefreshPresenceResponse{}), nil
}

type serverService struct {
	apiv1connect.UnimplementedServerServiceHandler
	s *Server
}

func (sv *serverService) GetRuntimeConfig(context.Context, *connect.Request[apiv1.GetRuntimeConfigRequest]) (*connect.Response[apiv1.GetRuntimeConfigResponse], error) {
	return connect.NewResponse(&apiv1.GetRuntimeConfigResponse{Runtime: &apiv1.ServerRuntimeConfig{MaxUploadSize: sv.s.MaxUploadSize}}), nil
}

type userService struct {
	apiv1connect.UnimplementedUserServiceHandler
	s *Server
}

func (us *userService) GetUser(_ context.Context, req *connect.Request[apiv1.GetUserRequest]) (*connect.Response[apiv1.GetUserResponse], error) {
	us.s.mu.Lock()
	defer us.s.mu.Unlock()
	for _, u := range us.s.users {
		if u.ID == req.Msg.GetUserId() || (req.Msg.GetLogin() != "" && strings.EqualFold(u.Login, req.Msg.GetLogin())) {
			return connect.NewResponse(&apiv1.GetUserResponse{User: &apiv1.DirectoryMember{User: us.s.userProtoLocked(u)}}), nil
		}
	}
	return nil, errNotFound("user")
}

func (us *userService) BatchGetUsers(_ context.Context, req *connect.Request[apiv1.BatchGetUsersRequest]) (*connect.Response[apiv1.BatchGetUsersResponse], error) {
	us.s.mu.Lock()
	defer us.s.mu.Unlock()
	resp := &apiv1.BatchGetUsersResponse{}
	for _, id := range req.Msg.GetUserIds() {
		if u, ok := us.s.users[id]; ok {
			resp.Users = append(resp.Users, &apiv1.DirectoryMember{User: us.s.userProtoLocked(u)})
		}
	}
	return connect.NewResponse(resp), nil
}

func (us *userService) ListUsers(_ context.Context, req *connect.Request[apiv1.ListUsersRequest]) (*connect.Response[apiv1.ListUsersResponse], error) {
	us.s.mu.Lock()
	defer us.s.mu.Unlock()
	resp := &apiv1.ListUsersResponse{Page: &apiv1.PageInfo{}}
	q := strings.ToLower(req.Msg.GetSearch())
	ids := make([]string, 0, len(us.s.users))
	for id := range us.s.users {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		u := us.s.users[id]
		if q == "" || strings.Contains(strings.ToLower(u.Login+" "+u.DisplayName), q) {
			resp.Users = append(resp.Users, &apiv1.DirectoryMember{User: us.s.userProtoLocked(u)})
		}
	}
	resp.Page.TotalCount = int64(len(resp.Users))
	return connect.NewResponse(resp), nil
}

type directoryService struct {
	apiv1connect.UnimplementedRoomDirectoryServiceHandler
	s *Server
}

func (d *directoryService) GetRoom(ctx context.Context, req *connect.Request[apiv1.GetRoomRequest]) (*connect.Response[apiv1.GetRoomResponse], error) {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	r, ok := d.s.rooms[req.Msg.GetRoomId()]
	if !ok {
		return nil, errNotFound("room")
	}
	uid := viewerID(ctx)
	if r.Kind == apiv1.RoomKind_ROOM_KIND_DM && !slices.Contains(r.Members, uid) {
		return nil, errDenied("hidden")
	}
	return connect.NewResponse(&apiv1.GetRoomResponse{Room: d.s.roomWithViewerLocked(r, uid)}), nil
}

func (d *directoryService) ListRooms(ctx context.Context, req *connect.Request[apiv1.ListRoomsRequest]) (*connect.Response[apiv1.ListRoomsResponse], error) {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	uid := viewerID(ctx)
	var all []*apiv1.RoomWithViewerState
	for _, r := range d.s.sortedRoomsLocked() {
		if slices.Contains(r.Members, uid) && !r.Archived {
			all = append(all, d.s.roomWithViewerLocked(r, uid))
		}
	}
	offset := int(req.Msg.GetPage().GetOffset())
	limit := int(req.Msg.GetPage().GetLimit())
	if limit <= 0 {
		limit = 50
	}
	end := min(offset+limit, len(all))
	offset = min(offset, end)
	return connect.NewResponse(&apiv1.ListRoomsResponse{
		Rooms: all[offset:end],
		Page:  &apiv1.PageInfo{TotalCount: int64(len(all)), HasMore: end < len(all)},
	}), nil
}

func (d *directoryService) GetRoomGroup(ctx context.Context, req *connect.Request[apiv1.GetRoomGroupRequest]) (*connect.Response[apiv1.GetRoomGroupResponse], error) {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	g, ok := d.s.groups[req.Msg.GetGroupId()]
	if !ok {
		return nil, errNotFound("group")
	}
	return connect.NewResponse(&apiv1.GetRoomGroupResponse{Group: d.s.groupLocked(g, viewerID(ctx))}), nil
}

func (d *directoryService) ListRoomGroups(ctx context.Context, _ *connect.Request[apiv1.ListRoomGroupsRequest]) (*connect.Response[apiv1.ListRoomGroupsResponse], error) {
	d.s.mu.Lock()
	defer d.s.mu.Unlock()
	resp := &apiv1.ListRoomGroupsResponse{}
	for _, gid := range d.s.groupOrder {
		resp.Groups = append(resp.Groups, d.s.groupLocked(d.s.groups[gid], viewerID(ctx)))
	}
	return connect.NewResponse(resp), nil
}

type roomService struct {
	apiv1connect.UnimplementedRoomServiceHandler
	s *Server
}

func (rs *roomService) GetRoomEvents(ctx context.Context, req *connect.Request[apiv1.GetRoomEventsRequest]) (*connect.Response[apiv1.GetRoomEventsResponse], error) {
	rs.s.mu.Lock()
	defer rs.s.mu.Unlock()
	uid := viewerID(ctx)
	if _, err := rs.s.memberRoomLocked(req.Msg.GetRoomId(), uid); err != nil {
		return nil, err
	}
	ids := rs.s.timeline[req.Msg.GetRoomId()]
	limit := limitOr(req.Msg.GetLimit())
	start, end := len(ids)-limit, len(ids)
	if c, ok := parseCursor(req.Msg.GetBefore()); ok {
		start, end = c-limit, c
	} else if c, ok := parseCursor(req.Msg.GetAfter()); ok {
		start, end = c, c+limit
	}
	return connect.NewResponse(&apiv1.GetRoomEventsResponse{Page: rs.s.pageLocked(ids, start, end, uid, nil)}), nil
}

func (rs *roomService) GetRoomEventsAround(ctx context.Context, req *connect.Request[apiv1.GetRoomEventsAroundRequest]) (*connect.Response[apiv1.GetRoomEventsAroundResponse], error) {
	rs.s.mu.Lock()
	defer rs.s.mu.Unlock()
	uid := viewerID(ctx)
	if _, err := rs.s.memberRoomLocked(req.Msg.GetRoomId(), uid); err != nil {
		return nil, err
	}
	ids := rs.s.timeline[req.Msg.GetRoomId()]
	idx := slices.Index(ids, req.Msg.GetEventId())
	if idx < 0 {
		return nil, errNotFound("event")
	}
	limit := limitOr(req.Msg.GetLimit())
	start := max(idx-limit/2, 0)
	page := rs.s.pageLocked(ids, start, start+limit, uid, nil)
	return connect.NewResponse(&apiv1.GetRoomEventsAroundResponse{Page: page, TargetIndex: int32(idx - start)}), nil
}

func (rs *roomService) ListMembers(ctx context.Context, req *connect.Request[apiv1.ListMembersRequest]) (*connect.Response[apiv1.ListMembersResponse], error) {
	rs.s.mu.Lock()
	defer rs.s.mu.Unlock()
	r, ok := rs.s.rooms[req.Msg.GetRoomId()]
	if !ok {
		return nil, errNotFound("room")
	}
	members := slices.Clone(r.Members)
	slices.Sort(members)
	offset := int(req.Msg.GetPage().GetOffset())
	limit := int(req.Msg.GetPage().GetLimit())
	if limit <= 0 {
		limit = 250
	}
	end := min(offset+limit, len(members))
	offset = min(offset, end)
	return connect.NewResponse(&apiv1.ListMembersResponse{
		UserIds: members[offset:end],
		Page:    &apiv1.PageInfo{TotalCount: int64(len(members)), HasMore: end < len(members)},
	}), nil
}

func (rs *roomService) StartDM(ctx context.Context, req *connect.Request[apiv1.StartDMRequest]) (*connect.Response[apiv1.StartDMResponse], error) {
	uid := viewerID(ctx)
	rs.s.mu.Lock()
	if rs.s.users[uid].BotOwner != "" {
		rs.s.mu.Unlock()
		return nil, errDenied("bots cannot start DMs")
	}
	want := append([]string{uid}, req.Msg.GetParticipantIds()...)
	slices.Sort(want)
	for _, r := range rs.s.rooms {
		if r.Kind == apiv1.RoomKind_ROOM_KIND_DM {
			have := slices.Clone(r.Members)
			slices.Sort(have)
			if slices.Equal(have, want) {
				defer rs.s.mu.Unlock()
				return connect.NewResponse(&apiv1.StartDMResponse{Room: rs.s.roomProto(r)}), nil
			}
		}
	}
	rs.s.mu.Unlock()
	if len(want) > 2 {
		return nil, errInvalid("new group DMs are not supported")
	}
	r := rs.s.AddDM(want...)
	return connect.NewResponse(&apiv1.StartDMResponse{Room: rs.s.roomProto(r)}), nil
}

func (rs *roomService) MarkRoomAsRead(ctx context.Context, req *connect.Request[apiv1.MarkRoomAsReadRequest]) (*connect.Response[apiv1.MarkRoomAsReadResponse], error) {
	rs.s.mu.Lock()
	defer rs.s.mu.Unlock()
	target := req.Msg.GetUpToEventId()
	if target == "" {
		if ids := rs.s.timeline[req.Msg.GetRoomId()]; len(ids) > 0 {
			target = ids[len(ids)-1]
		}
	}
	rs.s.readMarkers[viewerID(ctx)+"/"+req.Msg.GetRoomId()] = target
	return connect.NewResponse(&apiv1.MarkRoomAsReadResponse{}), nil
}

func (rs *roomService) GetRoomReadState(ctx context.Context, req *connect.Request[apiv1.GetRoomReadStateRequest]) (*connect.Response[apiv1.GetRoomReadStateResponse], error) {
	rs.s.mu.Lock()
	defer rs.s.mu.Unlock()
	state := &apiv1.RoomReadState{RoomId: req.Msg.GetRoomId()}
	if id := rs.s.readMarkers[viewerID(ctx)+"/"+req.Msg.GetRoomId()]; id != "" {
		state.Marker = &apiv1.ReadMarker{LastReadEventId: id, LastReadAt: timestamppb.Now()}
	}
	return connect.NewResponse(&apiv1.GetRoomReadStateResponse{State: state}), nil
}

func (rs *roomService) RefreshTypingIndicator(ctx context.Context, req *connect.Request[apiv1.RefreshTypingIndicatorRequest]) (*connect.Response[apiv1.RefreshTypingIndicatorResponse], error) {
	rs.s.mu.Lock()
	rs.s.TypingCalls = append(rs.s.TypingCalls, viewerID(ctx)+"/"+req.Msg.GetRoomId())
	rs.s.mu.Unlock()
	return connect.NewResponse(&apiv1.RefreshTypingIndicatorResponse{}), nil
}

func (rs *roomService) UpdateRoom(ctx context.Context, req *connect.Request[apiv1.UpdateRoomRequest]) (*connect.Response[apiv1.UpdateRoomResponse], error) {
	rs.s.mu.Lock()
	r, err := rs.s.memberRoomLocked(req.Msg.GetRoomId(), viewerID(ctx))
	if err != nil {
		rs.s.mu.Unlock()
		return nil, err
	}
	for _, path := range req.Msg.GetUpdateMask().GetPaths() {
		switch path {
		case "name":
			r.Name = req.Msg.GetName()
		case "description":
			r.Description = req.Msg.GetDescription()
		}
	}
	name, desc := r.Name, r.Description
	resp := &apiv1.UpdateRoomResponse{Room: rs.s.roomProto(r)}
	rs.s.mu.Unlock()
	rs.s.Publish(r.ID, viewerID(ctx), &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_RoomUpdated{
		RoomUpdated: &realtimev1.RoomUpdatedEvent{RoomId: r.ID, Name: name, Description: desc},
	}})
	return connect.NewResponse(resp), nil
}

func (rs *roomService) LeaveRoom(ctx context.Context, req *connect.Request[apiv1.LeaveRoomRequest]) (*connect.Response[apiv1.LeaveRoomResponse], error) {
	uid := viewerID(ctx)
	rs.s.mu.Lock()
	r, err := rs.s.memberRoomLocked(req.Msg.GetRoomId(), uid)
	if err != nil {
		rs.s.mu.Unlock()
		return nil, err
	}
	rs.s.mu.Unlock()
	rs.s.Publish(r.ID, uid, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_UserLeftRoom{UserLeftRoom: &realtimev1.UserLeftRoomEvent{RoomId: r.ID}}})
	rs.s.mu.Lock()
	r.Members = slices.DeleteFunc(r.Members, func(m string) bool { return m == uid })
	rs.s.mu.Unlock()
	return connect.NewResponse(&apiv1.LeaveRoomResponse{}), nil
}

// AddMember adds a user to a channel and publishes the join.
func (s *Server) AddMember(roomID, userID string) {
	s.mu.Lock()
	r := s.rooms[roomID]
	if !slices.Contains(r.Members, userID) {
		r.Members = append(r.Members, userID)
	}
	s.mu.Unlock()
	s.Publish(roomID, userID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_UserJoinedRoom{UserJoinedRoom: &realtimev1.UserJoinedRoomEvent{RoomId: roomID}}})
}

func (rs *roomService) AddMember(ctx context.Context, req *connect.Request[apiv1.AddMemberRequest]) (*connect.Response[apiv1.AddMemberResponse], error) {
	rs.s.mu.Lock()
	_, err := rs.s.memberRoomLocked(req.Msg.GetRoomId(), viewerID(ctx))
	rs.s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	rs.s.AddMember(req.Msg.GetRoomId(), req.Msg.GetUserId())
	return connect.NewResponse(&apiv1.AddMemberResponse{}), nil
}

func (rs *roomService) RemoveMember(ctx context.Context, req *connect.Request[apiv1.RemoveMemberRequest]) (*connect.Response[apiv1.RemoveMemberResponse], error) {
	rs.s.mu.Lock()
	r, err := rs.s.memberRoomLocked(req.Msg.GetRoomId(), viewerID(ctx))
	if err != nil {
		rs.s.mu.Unlock()
		return nil, err
	}
	r.Members = slices.DeleteFunc(r.Members, func(m string) bool { return m == req.Msg.GetUserId() })
	rs.s.mu.Unlock()
	rs.s.Publish(r.ID, req.Msg.GetUserId(), &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_UserLeftRoom{UserLeftRoom: &realtimev1.UserLeftRoomEvent{RoomId: r.ID}}})
	return connect.NewResponse(&apiv1.RemoveMemberResponse{}), nil
}

type threadService struct {
	apiv1connect.UnimplementedThreadServiceHandler
	s *Server
}

func (ts *threadService) GetThreadEvents(ctx context.Context, req *connect.Request[apiv1.GetThreadEventsRequest]) (*connect.Response[apiv1.GetThreadEventsResponse], error) {
	ts.s.mu.Lock()
	defer ts.s.mu.Unlock()
	uid := viewerID(ctx)
	if _, err := ts.s.memberRoomLocked(req.Msg.GetRoomId(), uid); err != nil {
		return nil, err
	}
	root, ok := ts.s.messages[req.Msg.GetThreadRootEventId()]
	if !ok {
		return nil, errNotFound("thread")
	}
	ids := ts.s.threads[root.ID]
	limit := limitOr(req.Msg.GetLimit())
	start, end := len(ids)-limit, len(ids)
	var prefix []*Message
	if c, ok := parseCursor(req.Msg.GetBefore()); ok {
		start, end = c-limit, c
	} else if c, ok := parseCursor(req.Msg.GetAfter()); ok {
		start, end = c, c+limit
	} else {
		prefix = []*Message{root}
	}
	return connect.NewResponse(&apiv1.GetThreadEventsResponse{Page: ts.s.pageLocked(ids, start, end, uid, prefix)}), nil
}

func (ts *threadService) GetThreadEventsAround(ctx context.Context, req *connect.Request[apiv1.GetThreadEventsAroundRequest]) (*connect.Response[apiv1.GetThreadEventsAroundResponse], error) {
	ts.s.mu.Lock()
	defer ts.s.mu.Unlock()
	uid := viewerID(ctx)
	ids := ts.s.threads[req.Msg.GetThreadRootEventId()]
	idx := slices.Index(ids, req.Msg.GetEventId())
	if idx < 0 {
		return nil, errNotFound("event")
	}
	limit := limitOr(req.Msg.GetLimit())
	start := max(idx-limit/2, 0)
	return connect.NewResponse(&apiv1.GetThreadEventsAroundResponse{Page: ts.s.pageLocked(ids, start, start+limit, uid, nil), TargetIndex: int32(idx - start)}), nil
}

func (ts *threadService) MarkThreadAsRead(ctx context.Context, req *connect.Request[apiv1.MarkThreadAsReadRequest]) (*connect.Response[apiv1.MarkThreadAsReadResponse], error) {
	ts.s.mu.Lock()
	defer ts.s.mu.Unlock()
	ts.s.readMarkers[viewerID(ctx)+"/"+req.Msg.GetRoomId()+"/"+req.Msg.GetThreadRootEventId()] = req.Msg.GetUpToEventId()
	return connect.NewResponse(&apiv1.MarkThreadAsReadResponse{}), nil
}

type messageService struct {
	apiv1connect.UnimplementedMessageServiceHandler
	s *Server
}

func (ms *messageService) CreateMessage(ctx context.Context, req *connect.Request[apiv1.CreateMessageRequest]) (*connect.Response[apiv1.CreateMessageResponse], error) {
	uid := viewerID(ctx)
	in := req.Msg
	ms.s.mu.Lock()
	room, err := ms.s.memberRoomLocked(in.GetRoomId(), uid)
	if err != nil {
		ms.s.mu.Unlock()
		return nil, err
	}
	if room.Archived {
		ms.s.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("room is archived"))
	}
	if len(in.GetBody()) > 10000 {
		ms.s.mu.Unlock()
		return nil, errInvalid("message too long")
	}
	if strings.TrimSpace(in.GetBody()) == "" && len(in.GetAttachmentAssetIds()) == 0 {
		ms.s.mu.Unlock()
		return nil, errInvalid("empty message")
	}
	if key := in.GetIdempotencyKey(); key != "" {
		if id, ok := ms.s.idem[uid+"/"+key]; ok {
			resp := ms.s.messageProtoLocked(ms.s.messages[id], uid)
			ms.s.mu.Unlock()
			return connect.NewResponse(&apiv1.CreateMessageResponse{Message: resp}), nil
		}
	}
	if root := in.GetThreadRootEventId(); root != "" {
		rm, ok := ms.s.messages[root]
		if !ok || rm.ThreadRoot != "" {
			ms.s.mu.Unlock()
			return nil, errInvalid("thread root must be a root message")
		}
		if room.ThreadingMode == apiv1.RoomThreadingMode_ROOM_THREADING_MODE_DISABLED || room.Kind == apiv1.RoomKind_ROOM_KIND_DM {
			ms.s.mu.Unlock()
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("threads are disabled"))
		}
	}
	m := &Message{
		ID: ms.s.newID("E"), RoomID: in.GetRoomId(), ActorID: uid, Body: in.GetBody(), CreatedAt: time.Now(),
		InReplyTo: in.GetInReplyTo(), ThreadRoot: in.GetThreadRootEventId(), Reactions: map[string][]string{},
	}
	for _, aid := range in.GetAttachmentAssetIds() {
		a, ok := ms.s.assets[aid]
		if !ok {
			ms.s.mu.Unlock()
			return nil, errInvalid("unknown asset " + aid)
		}
		m.Attachments = append(m.Attachments, a)
	}
	ms.s.storeMessageLocked(m)
	if key := in.GetIdempotencyKey(); key != "" {
		ms.s.idem[uid+"/"+key] = m.ID
	}
	resp := ms.s.messageProtoLocked(m, uid)
	delay := ms.s.PublishBeforeResponse
	ms.s.mu.Unlock()
	ms.s.publishMessagePosted(m)
	if delay > 0 {
		time.Sleep(delay)
	}
	return connect.NewResponse(&apiv1.CreateMessageResponse{Message: resp}), nil
}

func (ms *messageService) ownMessage(ctx context.Context, roomID, eventID string) (*Message, error) {
	m, ok := ms.s.messages[eventID]
	if !ok || m.RoomID != roomID || !m.DeletedAt.IsZero() {
		return nil, errNotFound("message")
	}
	if m.ActorID != viewerID(ctx) {
		return nil, errDenied("not your message")
	}
	return m, nil
}

func (ms *messageService) UpdateMessage(ctx context.Context, req *connect.Request[apiv1.UpdateMessageRequest]) (*connect.Response[apiv1.UpdateMessageResponse], error) {
	ms.s.mu.Lock()
	m, err := ms.ownMessage(ctx, req.Msg.GetRoomId(), req.Msg.GetEventId())
	if err != nil {
		ms.s.mu.Unlock()
		return nil, err
	}
	if !slices.Contains(req.Msg.GetUpdateMask().GetPaths(), "body") {
		ms.s.mu.Unlock()
		return nil, errInvalid("update_mask must contain body")
	}
	m.Body = req.Msg.GetBody()
	m.UpdatedAt = time.Now()
	resp := ms.s.messageProtoLocked(m, viewerID(ctx))
	ms.s.mu.Unlock()
	ms.s.Publish(m.RoomID, m.ActorID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_MessageEdited{
		MessageEdited: &realtimev1.MessageEditedEvent{RoomId: m.RoomID, MessageEventId: m.ID},
	}})
	return connect.NewResponse(&apiv1.UpdateMessageResponse{Message: resp}), nil
}

func (ms *messageService) DeleteMessage(ctx context.Context, req *connect.Request[apiv1.DeleteMessageRequest]) (*connect.Response[apiv1.DeleteMessageResponse], error) {
	ms.s.mu.Lock()
	m, err := ms.ownMessage(ctx, req.Msg.GetRoomId(), req.Msg.GetEventId())
	if err != nil {
		ms.s.mu.Unlock()
		return nil, err
	}
	m.DeletedAt = time.Now()
	ms.s.mu.Unlock()
	ms.s.Publish(m.RoomID, m.ActorID, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_MessageRetracted{
		MessageRetracted: &realtimev1.MessageRetractedEvent{RoomId: m.RoomID, MessageEventId: m.ID},
	}})
	return connect.NewResponse(&apiv1.DeleteMessageResponse{}), nil
}

func (ms *messageService) DeleteAttachment(ctx context.Context, req *connect.Request[apiv1.DeleteAttachmentRequest]) (*connect.Response[apiv1.DeleteAttachmentResponse], error) {
	ms.s.mu.Lock()
	m, err := ms.ownMessage(ctx, req.Msg.GetRoomId(), req.Msg.GetEventId())
	if err != nil {
		ms.s.mu.Unlock()
		return nil, err
	}
	m.Attachments = slices.DeleteFunc(m.Attachments, func(a *Attachment) bool { return a.ID == req.Msg.GetAttachmentId() })
	ms.s.mu.Unlock()
	return connect.NewResponse(&apiv1.DeleteAttachmentResponse{}), nil
}

func (ms *messageService) GetMessage(ctx context.Context, req *connect.Request[apiv1.GetMessageRequest]) (*connect.Response[apiv1.GetMessageResponse], error) {
	ms.s.mu.Lock()
	defer ms.s.mu.Unlock()
	m, ok := ms.s.messages[req.Msg.GetEventId()]
	if !ok || m.RoomID != req.Msg.GetRoomId() || !m.DeletedAt.IsZero() {
		return nil, errNotFound("message")
	}
	if _, err := ms.s.memberRoomLocked(m.RoomID, viewerID(ctx)); err != nil {
		return nil, err
	}
	return connect.NewResponse(&apiv1.GetMessageResponse{Message: ms.s.messageProtoLocked(m, viewerID(ctx))}), nil
}

func (ms *messageService) AddReaction(ctx context.Context, req *connect.Request[apiv1.AddReactionRequest]) (*connect.Response[apiv1.AddReactionResponse], error) {
	if !emoji.IsKnownShortcode(req.Msg.GetEmoji()) {
		return nil, errInvalid("unsupported emoji")
	}
	uid := viewerID(ctx)
	ms.s.mu.Lock()
	m, ok := ms.s.messages[req.Msg.GetMessageEventId()]
	if !ok {
		ms.s.mu.Unlock()
		return nil, errNotFound("message")
	}
	added := addReactionLocked(m, uid, req.Msg.GetEmoji())
	ms.s.mu.Unlock()
	if added {
		ms.s.Publish(m.RoomID, uid, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_ReactionAdded{
			ReactionAdded: &realtimev1.ReactionAddedEvent{RoomId: m.RoomID, MessageEventId: m.ID, Emoji: req.Msg.GetEmoji()},
		}})
	}
	return connect.NewResponse(&apiv1.AddReactionResponse{Added: added}), nil
}

func (ms *messageService) RemoveReaction(ctx context.Context, req *connect.Request[apiv1.RemoveReactionRequest]) (*connect.Response[apiv1.RemoveReactionResponse], error) {
	uid := viewerID(ctx)
	ms.s.mu.Lock()
	m, ok := ms.s.messages[req.Msg.GetMessageEventId()]
	if !ok {
		ms.s.mu.Unlock()
		return nil, errNotFound("message")
	}
	before := len(m.Reactions[req.Msg.GetEmoji()])
	m.Reactions[req.Msg.GetEmoji()] = slices.DeleteFunc(m.Reactions[req.Msg.GetEmoji()], func(u string) bool { return u == uid })
	removed := len(m.Reactions[req.Msg.GetEmoji()]) < before
	ms.s.mu.Unlock()
	if removed {
		ms.s.Publish(m.RoomID, uid, &realtimev1.RealtimeEvent{Event: &realtimev1.RealtimeEvent_ReactionRemoved{
			ReactionRemoved: &realtimev1.ReactionRemovedEvent{RoomId: m.RoomID, MessageEventId: m.ID, Emoji: req.Msg.GetEmoji()},
		}})
	}
	return connect.NewResponse(&apiv1.RemoveReactionResponse{Removed: removed}), nil
}

func (ms *messageService) ListReactionUsers(_ context.Context, req *connect.Request[apiv1.ListReactionUsersRequest]) (*connect.Response[apiv1.ListReactionUsersResponse], error) {
	ms.s.mu.Lock()
	defer ms.s.mu.Unlock()
	m, ok := ms.s.messages[req.Msg.GetMessageEventId()]
	if !ok {
		return nil, errNotFound("message")
	}
	users := slices.Clone(m.Reactions[req.Msg.GetEmoji()])
	slices.Sort(users)
	offset := int(req.Msg.GetPage().GetOffset())
	limit := int(req.Msg.GetPage().GetLimit())
	if limit <= 0 {
		limit = 50
	}
	end := min(offset+limit, len(users))
	offset = min(offset, end)
	return connect.NewResponse(&apiv1.ListReactionUsersResponse{UserIds: users[offset:end], Page: &apiv1.PageInfo{HasMore: end < len(users)}}), nil
}

type uploadService struct {
	apiv1connect.UnimplementedAssetUploadServiceHandler
	s *Server
}

func (us *uploadService) CreateUpload(ctx context.Context, req *connect.Request[apiv1.CreateUploadRequest]) (*connect.Response[apiv1.CreateUploadResponse], error) {
	if req.Msg.GetSize() > us.s.MaxUploadSize {
		return nil, errInvalid("file too large")
	}
	us.s.mu.Lock()
	defer us.s.mu.Unlock()
	if _, err := us.s.memberRoomLocked(req.Msg.GetRoomId(), viewerID(ctx)); err != nil {
		return nil, err
	}
	u := &upload{id: us.s.newID("P"), roomID: req.Msg.GetRoomId(), filename: req.Msg.GetFilename(),
		contentType: req.Msg.GetContentType(), size: req.Msg.GetSize(), sha: req.Msg.GetSha256()}
	us.s.uploads[u.id] = u
	return connect.NewResponse(&apiv1.CreateUploadResponse{Upload: us.uploadProto(u)}), nil
}

func (us *uploadService) uploadProto(u *upload) *apiv1.AssetUpload {
	return &apiv1.AssetUpload{UploadId: u.id, RoomId: u.roomID, CommittedOffset: int64(len(u.data)), Size: u.size,
		MaxChunkSize: us.s.MaxChunkSize, Sha256: u.sha, AssetId: u.assetID}
}

func (us *uploadService) UploadChunk(_ context.Context, req *connect.Request[apiv1.UploadChunkRequest]) (*connect.Response[apiv1.UploadChunkResponse], error) {
	us.s.mu.Lock()
	defer us.s.mu.Unlock()
	u, ok := us.s.uploads[req.Msg.GetUploadId()]
	if !ok {
		return nil, errNotFound("upload")
	}
	if req.Msg.GetOffset() != int64(len(u.data)) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("offset mismatch"))
	}
	if len(req.Msg.GetContent()) > int(us.s.MaxChunkSize) {
		return nil, errInvalid("chunk too large")
	}
	sum := sha256.Sum256(req.Msg.GetContent())
	if hex.EncodeToString(sum[:]) != req.Msg.GetChunkSha256() {
		return nil, errInvalid("chunk checksum mismatch")
	}
	u.data = append(u.data, req.Msg.GetContent()...)
	return connect.NewResponse(&apiv1.UploadChunkResponse{Upload: us.uploadProto(u)}), nil
}

func (us *uploadService) CompleteUpload(_ context.Context, req *connect.Request[apiv1.CompleteUploadRequest]) (*connect.Response[apiv1.CompleteUploadResponse], error) {
	us.s.mu.Lock()
	defer us.s.mu.Unlock()
	u, ok := us.s.uploads[req.Msg.GetUploadId()]
	if !ok {
		return nil, errNotFound("upload")
	}
	sum := sha256.Sum256(u.data)
	if hex.EncodeToString(sum[:]) != u.sha || int64(len(u.data)) != u.size {
		return nil, errInvalid("file checksum mismatch")
	}
	a := &Attachment{ID: us.s.newID("A"), Filename: u.filename, ContentType: u.contentType, Data: u.data}
	us.s.assets[a.ID] = a
	u.assetID = a.ID
	return connect.NewResponse(&apiv1.CompleteUploadResponse{Upload: us.uploadProto(u), Asset: &apiv1.Asset{Id: a.ID, Filename: a.Filename}}), nil
}

func (us *uploadService) CancelUpload(_ context.Context, req *connect.Request[apiv1.CancelUploadRequest]) (*connect.Response[apiv1.CancelUploadResponse], error) {
	us.s.mu.Lock()
	delete(us.s.uploads, req.Msg.GetUploadId())
	us.s.mu.Unlock()
	return connect.NewResponse(&apiv1.CancelUploadResponse{}), nil
}

// Asset returns an uploaded asset.
func (s *Server) Asset(id string) *Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.assets[id]
}

type invitationService struct {
	apiv1connect.UnimplementedRoomInvitationServiceHandler
	s *Server
}

// AcceptRoomInvitation treats the invitation ID as the room ID.
func (is *invitationService) AcceptRoomInvitation(ctx context.Context, req *connect.Request[apiv1.AcceptRoomInvitationRequest]) (*connect.Response[apiv1.AcceptRoomInvitationResponse], error) {
	roomID := req.Msg.GetInvitationId()
	is.s.mu.Lock()
	_, ok := is.s.rooms[roomID]
	is.s.mu.Unlock()
	if !ok {
		return nil, errNotFound("invitation")
	}
	is.s.AddMember(roomID, viewerID(ctx))
	return connect.NewResponse(&apiv1.AcceptRoomInvitationResponse{Invitation: &apiv1.RoomInvitation{Id: roomID, RoomId: roomID}}), nil
}

type adminPermissionService struct {
	adminv1connect.UnimplementedAdminPermissionServiceHandler
	s *Server
}

func (a *adminPermissionService) SetUserPermission(ctx context.Context, req *connect.Request[adminv1.SetUserPermissionRequest]) (*connect.Response[adminv1.SetUserPermissionResponse], error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	target, ok := a.s.users[req.Msg.GetUserId()]
	if !ok {
		return nil, errNotFound("user")
	}
	if target.BotOwner != viewerID(ctx) {
		return nil, errDenied("not the bot owner")
	}
	a.s.Permissions = append(a.s.Permissions, Permission{
		UserID: req.Msg.GetUserId(), Permission: req.Msg.GetPermission(), Decision: int32(req.Msg.GetDecision()),
		ScopeKind: int32(req.Msg.GetScope().GetKind()), ScopeID: req.Msg.GetScope().GetId(),
	})
	return connect.NewResponse(&adminv1.SetUserPermissionResponse{}), nil
}

// PermissionsFor returns the SetUserPermission calls made for a user.
func (s *Server) PermissionsFor(userID string) []Permission {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Permission
	for _, p := range s.Permissions {
		if p.UserID == userID {
			out = append(out, p)
		}
	}
	return out
}

type botService struct {
	apiv1connect.UnimplementedBotServiceHandler
	s *Server
}

func (bs *botService) CreateBot(ctx context.Context, req *connect.Request[apiv1.CreateBotRequest]) (*connect.Response[apiv1.CreateBotResponse], error) {
	owner := viewerID(ctx)
	bs.s.mu.Lock()
	if bs.s.users[owner].BotOwner != "" {
		bs.s.mu.Unlock()
		return nil, errDenied("bot API keys cannot manage bots")
	}
	for _, u := range bs.s.users {
		if strings.EqualFold(u.Login, req.Msg.GetLogin()) {
			bs.s.mu.Unlock()
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("login taken"))
		}
	}
	bs.s.mu.Unlock()
	bot, key := bs.s.AddBot(req.Msg.GetLogin(), req.Msg.GetDisplayName(), owner)
	bs.s.mu.Lock()
	defer bs.s.mu.Unlock()
	return connect.NewResponse(&apiv1.CreateBotResponse{
		Bot:    &apiv1.Bot{User: bs.s.userProtoLocked(bot), OwnerUserId: owner},
		ApiKey: key,
	}), nil
}

// Bots returns all bot accounts.
func (s *Server) Bots() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []User
	for _, u := range s.users {
		if u.BotOwner != "" {
			out = append(out, *u)
		}
	}
	return out
}

func (us *userService) UpdateUserProfile(ctx context.Context, req *connect.Request[apiv1.UpdateUserProfileRequest]) (*connect.Response[apiv1.UpdateUserProfileResponse], error) {
	us.s.mu.Lock()
	defer us.s.mu.Unlock()
	u, ok := us.s.users[req.Msg.GetUserId()]
	if !ok {
		return nil, errNotFound("user")
	}
	if u.ID != viewerID(ctx) && u.BotOwner != viewerID(ctx) {
		return nil, errDenied("cannot edit this profile")
	}
	if req.Msg.DisplayName != nil {
		u.DisplayName = req.Msg.GetDisplayName()
	}
	return connect.NewResponse(&apiv1.UpdateUserProfileResponse{User: us.s.userProtoLocked(u)}), nil
}

func (rs *roomService) ListPinnedMessages(ctx context.Context, req *connect.Request[apiv1.ListPinnedMessagesRequest]) (*connect.Response[apiv1.ListPinnedMessagesResponse], error) {
	rs.s.mu.Lock()
	defer rs.s.mu.Unlock()
	if _, err := rs.s.memberRoomLocked(req.Msg.GetRoomId(), viewerID(ctx)); err != nil {
		return nil, err
	}
	resp := &apiv1.ListPinnedMessagesResponse{Page: &apiv1.PageInfo{}}
	for _, id := range rs.s.pins[req.Msg.GetRoomId()] {
		resp.PinnedMessages = append(resp.PinnedMessages, &apiv1.PinnedMessage{Message: rs.s.messageProtoLocked(rs.s.messages[id], viewerID(ctx))})
	}
	return connect.NewResponse(resp), nil
}
