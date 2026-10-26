package connector

import (
	"context"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/elsbrock/mautrix-chatto/pkg/chattoclient"
	apiv1 "github.com/elsbrock/mautrix-chatto/pkg/chattopb/chatto/api/v1"
)

// userCache caches Chatto users by ID and login for mention resolution and
// ghost info. Entries expire so that renames are eventually picked up even
// without a profile event.
type userCache struct {
	api *chattoclient.Client
	ttl time.Duration

	mu      sync.Mutex
	byID    map[string]cachedUser
	byLogin map[string]string // lowercased login → ID
	missing map[string]time.Time
}

type cachedUser struct {
	user    *apiv1.User
	fetched time.Time
}

func newUserCache(api *chattoclient.Client) *userCache {
	return &userCache{
		api:     api,
		ttl:     30 * time.Minute,
		byID:    make(map[string]cachedUser),
		byLogin: make(map[string]string),
		missing: make(map[string]time.Time),
	}
}

// Put stores users, e.g. from timeline includes or snapshots.
func (uc *userCache) Put(users ...*apiv1.User) {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	now := time.Now()
	for _, u := range users {
		if u.GetId() == "" {
			continue
		}
		if old, ok := uc.byID[u.GetId()]; ok && old.user.GetLogin() != u.GetLogin() {
			delete(uc.byLogin, strings.ToLower(old.user.GetLogin()))
		}
		uc.byID[u.GetId()] = cachedUser{user: u, fetched: now}
		if u.GetLogin() != "" {
			uc.byLogin[strings.ToLower(u.GetLogin())] = u.GetId()
		}
	}
}

// Invalidate drops a user so the next lookup refetches it.
func (uc *userCache) Invalidate(userID string) {
	uc.mu.Lock()
	defer uc.mu.Unlock()
	if old, ok := uc.byID[userID]; ok {
		delete(uc.byLogin, strings.ToLower(old.user.GetLogin()))
		delete(uc.byID, userID)
	}
}

// Get returns a user by ID, fetching it if needed.
func (uc *userCache) Get(ctx context.Context, userID string) (*apiv1.User, error) {
	uc.mu.Lock()
	if cu, ok := uc.byID[userID]; ok && time.Since(cu.fetched) < uc.ttl {
		uc.mu.Unlock()
		return cu.user, nil
	}
	uc.mu.Unlock()
	resp, err := uc.api.Users.GetUser(ctx, connect.NewRequest(&apiv1.GetUserRequest{
		Target: &apiv1.GetUserRequest_UserId{UserId: userID},
	}))
	if err != nil {
		return nil, err
	}
	user := resp.Msg.GetUser().GetUser()
	uc.Put(user)
	return user, nil
}

// GetByLogin resolves a login case-insensitively. Unknown logins are cached
// as missing for a minute because @-text that is not a user is common.
func (uc *userCache) GetByLogin(ctx context.Context, login string) (*apiv1.User, bool) {
	key := strings.ToLower(login)
	uc.mu.Lock()
	if id, ok := uc.byLogin[key]; ok {
		if cu, ok := uc.byID[id]; ok && time.Since(cu.fetched) < uc.ttl {
			uc.mu.Unlock()
			return cu.user, true
		}
	}
	if at, ok := uc.missing[key]; ok && time.Since(at) < time.Minute {
		uc.mu.Unlock()
		return nil, false
	}
	uc.mu.Unlock()
	resp, err := uc.api.Users.GetUser(ctx, connect.NewRequest(&apiv1.GetUserRequest{
		Target: &apiv1.GetUserRequest_Login{Login: login},
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			uc.mu.Lock()
			uc.missing[key] = time.Now()
			uc.mu.Unlock()
		}
		return nil, false
	}
	user := resp.Msg.GetUser().GetUser()
	uc.Put(user)
	return user, true
}
