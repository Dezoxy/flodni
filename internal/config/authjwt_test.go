package config

import "testing"

func TestAuthJWTSame(t *testing.T) {
	base := func() *AuthJWT {
		return &AuthJWT{Issuer: "i", Audience: "a", JWKSURL: "https://x.example/k", Header: "H", AllowedEmails: []string{"a@x.test"}}
	}
	if !base().Same(base()) || !(*AuthJWT)(nil).Same(nil) {
		t.Fatal("equal blocks differ")
	}
	if base().Same(nil) || (*AuthJWT)(nil).Same(base()) {
		t.Fatal("nil equals a block")
	}
	for name, edit := range map[string]func(*AuthJWT){
		"issuer":         func(a *AuthJWT) { a.Issuer = "j" },
		"audience":       func(a *AuthJWT) { a.Audience = "b" },
		"jwks":           func(a *AuthJWT) { a.JWKSURL = "https://y.example/k" },
		"header":         func(a *AuthJWT) { a.Header = "G" },
		"emails changed": func(a *AuthJWT) { a.AllowedEmails = []string{"b@x.test"} },
		"emails added":   func(a *AuthJWT) { a.AllowedEmails = append(a.AllowedEmails, "c@x.test") },
		"emails removed": func(a *AuthJWT) { a.AllowedEmails = nil },
	} {
		b := base()
		edit(b)
		if base().Same(b) || b.Same(base()) {
			t.Errorf("%s: blocks compare equal", name)
		}
	}
}
