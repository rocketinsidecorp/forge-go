// Package principalwrite puts the authenticated principal in a request
// context, where principalctx.Value and auth.FromContext read it. Only the
// authentication middleware (auth/authhttp) may import it, after resolving a
// session through the application's Resolver: architecture_test.go refuses
// any other importer, so auth.AuthorizeMFAChange only ever sees a principal
// that signed in (ADR 0052).
package principalwrite

import (
	"context"

	"github.com/rocketinsidecorp/forge-go/internal/principalctx/internal/key"
)

// With returns a copy of ctx carrying principal.
func With(ctx context.Context, principal any) context.Context {
	return context.WithValue(ctx, key.Principal{}, principal)
}
