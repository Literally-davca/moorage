package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"moorage/internal/store"
)

// LinkTTL is how long an invite or reset link works.
const LinkTTL = 48 * time.Hour

// Link kinds.
const (
	LinkInvite = "invite"
	LinkReset  = "reset"
)

// ErrUserNotFound is an unknown user id.
var ErrUserNotFound = errors.New("user not found")

var errLastAdmin = RefusedError{"Moorage needs at least one active local admin with two-factor sign-in, so it stays reachable when the identity provider is down. Make another local admin first."}

// UserInfo is a user as the admin list shows it. It never holds a hash or a secret.
type UserInfo struct {
	ID          int64      `json:"id"`
	Username    string     `json:"username"`
	DisplayName string     `json:"displayName"`
	Email       string     `json:"email"`
	Source      string     `json:"source"`
	Role        string     `json:"role"`
	Status      string     `json:"status"` // "active", "invited" or "disabled"
	TOTP        bool       `json:"totp"`
	LastLoginAt *time.Time `json:"lastLoginAt,omitempty"`
	CreatedBy   string     `json:"createdBy"`
	CreatedAt   time.Time  `json:"createdAt"`
	LinkKind    string     `json:"linkKind,omitempty"` // open "invite" or "reset" link
	LinkExpires *time.Time `json:"linkExpires,omitempty"`
}

// Link is a freshly issued invite or reset link token, shown to the admin once.
type Link struct {
	Token   string    `json:"token"`
	Kind    string    `json:"kind"`
	Expires time.Time `json:"expires"`
}

// ListUsers returns every account for the admin list.
func (s *Service) ListUsers(ctx context.Context) ([]UserInfo, error) {
	rows, err := s.db.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UserInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, userInfo(r))
	}
	return out, nil
}

func userInfo(r store.UserListEntry) UserInfo {
	i := UserInfo{
		ID: r.ID, Username: r.Username, DisplayName: r.DisplayName, Email: r.Email, Source: r.AuthSource,
		Role: r.Role, Status: userStatus(r.User), TOTP: r.TOTPSecret != "" && r.Active, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt, LinkKind: r.LinkKind,
	}
	if !r.LastLoginAt.IsZero() {
		t := r.LastLoginAt
		i.LastLoginAt = &t
	}
	if !r.LinkExpires.IsZero() {
		t := r.LinkExpires
		i.LinkExpires = &t
	}
	return i
}

func userStatus(u store.User) string {
	switch {
	case u.Disabled:
		return "disabled"
	case !u.Active:
		return "invited"
	}
	return "active"
}

func validRole(role string) bool { return role == "admin" || role == "viewer" }

// CreateUser adds a local user and returns its invite link. The admin never sets or sees
// a password: the invitee chooses it when opening the link.
func (s *Service) CreateUser(ctx context.Context, admin User, username, role, displayName string) (UserInfo, Link, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	displayName = strings.TrimSpace(displayName)
	if !usernameRe.MatchString(username) {
		return UserInfo{}, Link{}, ValidationError{usernameHelp}
	}
	if !validRole(role) {
		return UserInfo{}, Link{}, ValidationError{"Choose the role admin or viewer."}
	}
	if len([]rune(displayName)) > 64 {
		return UserInfo{}, Link{}, ValidationError{"Display name can have at most 64 characters."}
	}
	id, err := s.db.CreateInvitedUser(ctx, username, role, displayName, admin.Username)
	if errors.Is(err, store.ErrUsernameTaken) {
		return UserInfo{}, Link{}, ValidationError{"That username is taken."}
	}
	if err != nil {
		return UserInfo{}, Link{}, err
	}
	link, err := s.issueLink(ctx, id, LinkInvite, admin.Username)
	if err != nil {
		return UserInfo{}, Link{}, err
	}
	info, err := s.userInfoByID(ctx, id)
	return info, link, err
}

func (s *Service) issueLink(ctx context.Context, userID int64, kind, by string) (Link, error) {
	tok := randomHex(32)
	exp := s.now().Add(LinkTTL)
	if err := s.db.IssueUserToken(ctx, hashToken(tok), userID, kind, by, exp); err != nil {
		return Link{}, fmt.Errorf("issue %s link: %w", kind, err)
	}
	return Link{Token: tok, Kind: kind, Expires: exp}, nil
}

func (s *Service) userInfoByID(ctx context.Context, id int64) (UserInfo, error) {
	rows, err := s.db.ListUsers(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	for _, r := range rows {
		if r.ID == id {
			return userInfo(r), nil
		}
	}
	return UserInfo{}, ErrUserNotFound
}

// target loads the user an admin acts on and refuses the admin's own account: changes to
// yourself go through the Account page, and another admin must demote or remove you.
func (s *Service) target(ctx context.Context, admin User, id int64) (store.User, error) {
	u, err := s.db.UserByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return u, ErrUserNotFound
	}
	if err != nil {
		return u, err
	}
	if u.ID == admin.ID {
		return u, RefusedError{"You can't change your own account here. Use your Account page, or ask another admin."}
	}
	return u, nil
}

// SetRole changes a local user's role and ends their sessions. OIDC and proxy roles come
// from the groups at every sign-in, so they can't be changed here.
func (s *Service) SetRole(ctx context.Context, admin User, id int64, role string) (store.User, error) {
	if !validRole(role) {
		return store.User{}, ValidationError{"Choose the role admin or viewer."}
	}
	u, err := s.target(ctx, admin, id)
	if err != nil {
		return u, err
	}
	if u.AuthSource != store.SourceLocal {
		return u, RefusedError{"The role of this user comes from their groups in the identity provider. Change it there."}
	}
	if u.Role == role {
		return u, nil
	}
	return s.change(ctx, id, store.UserChange{Role: role})
}

// SetDisabled disables or enables a user. Disabling ends their sessions; it also wins
// over the IdP and the proxy, which can't sign a disabled user in.
func (s *Service) SetDisabled(ctx context.Context, admin User, id int64, disabled bool) (store.User, error) {
	if _, err := s.target(ctx, admin, id); err != nil {
		return store.User{}, err
	}
	return s.change(ctx, id, store.UserChange{Disable: &disabled})
}

// DeleteUser removes a user, their sessions and their open links.
func (s *Service) DeleteUser(ctx context.Context, admin User, id int64) (store.User, error) {
	if _, err := s.target(ctx, admin, id); err != nil {
		return store.User{}, err
	}
	return s.change(ctx, id, store.UserChange{Delete: true})
}

func (s *Service) change(ctx context.Context, id int64, c store.UserChange) (store.User, error) {
	u, err := s.db.ChangeUser(ctx, id, c)
	switch {
	case errors.Is(err, store.ErrLastAdmin):
		return u, errLastAdmin
	case errors.Is(err, store.ErrNotFound):
		return u, ErrUserNotFound
	}
	return u, err
}

// IssueReset gives a local user a new link. For an invited user it is a fresh invite
// (the old link stops working); for an active user a reset link, which replaces the
// password and TOTP only when it is finished. Their sessions end at that point, not now,
// so a reset issued by mistake changes nothing.
func (s *Service) IssueReset(ctx context.Context, admin User, id int64) (UserInfo, Link, error) {
	u, err := s.target(ctx, admin, id)
	if err != nil {
		return UserInfo{}, Link{}, err
	}
	if u.AuthSource != store.SourceLocal {
		return UserInfo{}, Link{}, RefusedError{"This user signs in through the identity provider and has no Moorage password."}
	}
	kind := LinkReset
	if !u.Active {
		kind = LinkInvite
	}
	link, err := s.issueLink(ctx, id, kind, admin.Username)
	if err != nil {
		return UserInfo{}, Link{}, err
	}
	info, err := s.userInfoByID(ctx, id)
	return info, link, err
}

// RevokeSessions signs a user out everywhere.
func (s *Service) RevokeSessions(ctx context.Context, admin User, id int64) (store.User, error) {
	u, err := s.target(ctx, admin, id)
	if err != nil {
		return u, err
	}
	_, err = s.db.DeleteUserSessions(ctx, id, "")
	return u, err
}
