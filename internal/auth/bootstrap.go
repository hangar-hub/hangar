package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// Bootstrap creates the first Instance admin from
// security.bootstrap_admin (AUTH-6) when no enabled admin exists. The
// default username is hangar-admin. It returns the User and whether it
// created one. It is safe to run on every start and on several replicas
// at once.
func (s *Service) Bootstrap(ctx context.Context) (*User, bool, error) {
	ba := s.cfg.Security.BootstrapAdmin
	n, err := s.db.NewSelect().Model((*db.User)(nil)).
		Where("is_admin = ? AND is_system = ? AND disabled_at IS NULL", true, false).Count(ctx)
	if err != nil {
		return nil, false, err
	}
	if n > 0 {
		return nil, false, nil
	}
	if ba.Username == "" {
		return nil, false, errors.New("no Instance admin exists and security.bootstrap_admin.username is empty")
	}
	if ba.Password == "" {
		return nil, false, errors.New("no Instance admin exists and security.bootstrap_admin.password is empty")
	}
	if existing, err := s.Users.GetByUsername(ctx, ba.Username); err == nil {
		return nil, false, fmt.Errorf("no Instance admin exists, but the bootstrap admin username %q belongs to a user who is not an enabled admin; "+
			"make them an admin with the hangar admin commands, or set security.bootstrap_admin.username to a free name", existing.Username)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	u, err := s.Users.Create(ctx, NewUser{Username: ba.Username, Password: ba.Password, Email: ba.Email, EmailVerified: ba.Email != "",
		IsAdmin: true, DisplayName: ba.Username}, model.System())
	if errors.Is(err, ErrConflict) {
		// Another replica created it at the same moment.
		u, gerr := s.Users.GetByUsername(ctx, ba.Username)
		if gerr == nil && u.IsAdmin {
			return u, false, nil
		}
		return nil, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("create the bootstrap admin %q: %w", ba.Username, err)
	}
	s.log.InfoContext(ctx, "created the bootstrap admin", "username", u.Username)
	return u, true, nil
}
