package auth

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/model"
)

// OrgMembershipView is one Organisation in MeView.
type OrgMembershipView struct {
	Name        string     `json:"name"`
	DisplayName string     `json:"display_name,omitempty"`
	Role        model.Role `json:"role"`
}

// MeView is the signed-in user, in the shape of the native API's Me
// schema. The /auth handlers and GET /me both return it.
type MeView struct {
	ID          uuid.UUID           `json:"id"`
	Username    string              `json:"username"`
	DisplayName string              `json:"display_name"`
	Email       string              `json:"email,omitempty"`
	AvatarURL   string              `json:"avatar_url,omitempty"`
	IsAdmin     bool                `json:"is_admin"`
	HasPassword bool                `json:"has_password"`
	Orgs        []OrgMembershipView `json:"orgs"`
	CSRFToken   string              `json:"csrf_token,omitempty"`
}

// Me builds the MeView for a signed-in Principal. csrf is the session's
// CSRF token, or "" for Token requests.
func (s *Service) Me(ctx context.Context, p model.Principal, csrf string) (*MeView, error) {
	if !p.IsUser() {
		return nil, fmt.Errorf("%w: not signed in", ErrInvalidCredentials)
	}
	u, err := s.Users.Get(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	ms, err := s.orgs.MembershipsOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	out := &MeView{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Email: u.Email, IsAdmin: u.IsAdmin, HasPassword: u.HasPassword,
		Orgs: make([]OrgMembershipView, 0, len(ms)), CSRFToken: csrf}
	if out.DisplayName == "" {
		out.DisplayName = u.Username
	}
	for _, m := range ms {
		out.Orgs = append(out.Orgs, OrgMembershipView{Name: m.Org.Name, DisplayName: m.Org.DisplayName, Role: m.Role})
	}
	return out, nil
}
