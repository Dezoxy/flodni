package apiauth

import "context"

// Identity is who a request was authenticated as. The gate puts it in the request
// context after it has authenticated, so the engine's router (the second half of the
// engine gate) and the handlers read it and never authenticate again.
type Identity struct {
	// Name is the token's name from the file, "legacy" for api_token, "jwt" for a
	// verified proxy assertion, "internal" for Flodni's own in-process calls.
	Name   string
	Scopes ScopeSet
	// Ambient is true when a browser attaches the credential by itself (a cookie or
	// the proxy's assertion), so the CSRF rules applied to it.
	Ambient bool
	// Internal is true for the in-process identity only.
	Internal bool
}

type identityKey struct{}

// WithIdentity returns ctx carrying id, replacing any identity already in it.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom reads the identity the gate authenticated, if there is one.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// internalName is the reserved token name no file may use.
const internalName = reservedName

// InternalIdentity is what Flodni's own calls into its engine carry: admin on the
// engine, and no token behind it. Only the in-process transport creates it.
func InternalIdentity() Identity {
	return Identity{Name: internalName, Scopes: scopeSetOf(AllScopes...), Internal: true}
}
