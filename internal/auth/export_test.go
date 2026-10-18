package auth

import (
	"context"

	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
)

// SignInWithClaims runs the OIDC sign-in decision for raw ID token claims,
// without the provider round trip, for tests of the account rules.
func (o *OIDCService) SignInWithClaims(ctx context.Context, cfg config.OIDCProvider, raw map[string]any) (*CallbackResult, error) {
	return o.signIn(ctx, cfg, mapClaims(cfg, raw), db.OIDCState{RedirectTo: "/"})
}
