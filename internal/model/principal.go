package model

import "context"

// Principal is the result of authenticating a WebDAV request. Acting is the
// user whose namespace is served; Auth is the identity that authenticated.
// They differ only during admin impersonation (Basic "admin/target").
type Principal struct {
	Acting *User
	Auth   *User
}

// IsAdmin reports whether the authenticated identity is an administrator.
func (p Principal) IsAdmin() bool { return p.Auth != nil && p.Auth.IsAdmin }

// Impersonating reports whether an admin is acting in another user's namespace.
func (p Principal) Impersonating() bool {
	return p.Acting != nil && p.Auth != nil && p.Acting.ID != p.Auth.ID
}

type principalCtxKey struct{}

// ContextWithPrincipal returns a copy of ctx carrying p.
func ContextWithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext extracts the principal stored by ContextWithPrincipal.
func PrincipalFromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok
}
