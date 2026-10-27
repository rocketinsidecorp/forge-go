// Package principalctx reads the authenticated principal from a request
// context. It is a leaf shared by auth, whose Principal is stored there
// (auth.FromContext), and by packages that only need to recognize a signed-in
// requester, such as broadcast, which must not import auth (ADR 0052).
//
// It cannot write one: principalctx/principalwrite does, and only
// auth/authhttp may import that (architecture_test.go). Being internal,
// neither is available to applications.
package principalctx

import (
	"context"

	"github.com/rocketinsidecorp/forge-go/internal/principalctx/internal/key"
)

// Value returns the principal stored in ctx, or nil. A nil ctx has none.
func Value(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	return ctx.Value(key.Principal{})
}
