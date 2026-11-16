package config

import (
	"errors"
	"net/url"
	"strings"
)

// AuthJWT is the optional proxy-signed identity (ADR 0004). The mode is off
// when the config.json block is absent. Read at start; a panel save never
// changes it.
type AuthJWT struct {
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
	JWKSURL  string `json:"jwks_url"`
	// Header is the request header that carries the assertion.
	Header string `json:"header"`
	// AllowedEmails, when non-empty, limits the mode to these addresses.
	AllowedEmails []string `json:"allowed_emails,omitempty"`
}

// Validate refuses a block that is incomplete, names the Authorization header
// (it would collide with the bearer rule) or points the key fetch off https.
func (a AuthJWT) Validate() error {
	if a.Issuer == "" || a.Audience == "" || a.JWKSURL == "" || a.Header == "" {
		return errors.New("auth_jwt needs issuer, audience, jwks_url and header")
	}
	if strings.EqualFold(strings.TrimSpace(a.Header), "Authorization") {
		return errors.New("auth_jwt header must not be Authorization")
	}
	if strings.ContainsAny(a.Header, " \t\r\n:") {
		return errors.New("auth_jwt header is not a valid header name")
	}
	u, err := url.Parse(a.JWKSURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("auth_jwt jwks_url must be an https URL")
	}
	for _, e := range a.AllowedEmails {
		if strings.TrimSpace(e) == "" {
			return errors.New("auth_jwt allowed_emails holds an empty entry")
		}
		for i := 0; i < len(e); i++ {
			if e[i] >= 0x80 {
				return errors.New("auth_jwt allowed_emails entries must be ASCII")
			}
		}
	}
	return nil
}

// Same reports whether two blocks are equal (a nil block and a nil one are).
func (a *AuthJWT) Same(b *AuthJWT) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Issuer != b.Issuer || a.Audience != b.Audience || a.JWKSURL != b.JWKSURL ||
		a.Header != b.Header || len(a.AllowedEmails) != len(b.AllowedEmails) {
		return false
	}
	for i := range a.AllowedEmails {
		if a.AllowedEmails[i] != b.AllowedEmails[i] {
			return false
		}
	}
	return true
}
