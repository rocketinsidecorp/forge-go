// Package key is the request-context key of the authenticated principal.
// Go's internal rule lets only principalctx (which reads it) and
// principalctx/principalwrite (which writes it) import it, so no other
// package can write a principal with context.WithValue (ADR 0052).
package key

// Principal is the context key the principal is stored under.
type Principal struct{}
