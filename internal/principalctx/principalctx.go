// Package principalctx holds the request context key of the authenticated
// principal. It is a leaf shared by auth, which stores and reads its
// Principal there (auth.FromContext), and by packages that only need to
// recognize a signed-in requester, such as broadcast, which must not import
// auth (ADR 0052). Being internal, applications cannot use it to forge a
// principal; they go through auth's middleware.
package principalctx

import "context"

type key struct{}

// With returns a copy of ctx carrying principal.
func With(ctx context.Context, principal any) context.Context {
	return context.WithValue(ctx, key{}, principal)
}

// Value returns the principal stored in ctx, or nil. A nil ctx has none.
func Value(ctx context.Context) any {
	if ctx == nil {
		return nil
	}
	return ctx.Value(key{})
}
