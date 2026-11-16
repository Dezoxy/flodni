// Package jwt verifies an identity assertion signed by a proxy (ADR 0004,
// "Proxy JWT mode"), with the standard library only. It accepts RS256 and
// ES256, picks the algorithm from the key and never from the token, and does
// nothing else: no signing, no other algorithm, no key from the token.
// Verify is not wired into request handling here.
package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"flodni/internal/config"
)

// Reasons for the refusal metric. A request with no header is ReasonMissing;
// the jwt_* reasons are for a header that is present.
const (
	ReasonMissing         = "missing"
	ReasonMalformed       = "jwt_malformed"
	ReasonAlg             = "jwt_alg"
	ReasonUnknownKid      = "jwt_unknown_kid"
	ReasonKeysUnavailable = "jwt_keys_unavailable"
	ReasonSignature       = "jwt_signature"
	ReasonIssuer          = "jwt_issuer"
	ReasonAudience        = "jwt_audience"
	ReasonExpired         = "jwt_expired"
	ReasonNotYetValid     = "jwt_not_yet_valid"
	ReasonClaims          = "jwt_claims"
)

const skew = 60 * time.Second

// Error carries the reason code. Its text never holds the token.
type Error struct{ Reason string }

func (e *Error) Error() string { return "jwt: " + e.Reason }

// Reason returns the reason code of an error from Verify ("" for any other).
func Reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

func fail(reason string) error { return &Error{reason} }

// Identity is what a verified assertion names.
type Identity struct {
	Subject string
	Email   string
}

// Verifier checks assertions against the keys of one JWKS.
type Verifier struct {
	cfg    config.AuthJWT
	emails map[string]bool // lower-cased allowed_emails; nil: no list
	now    func() time.Time
	keys   keyCache
}

// New refuses a configuration that Validate refuses. It does not fetch: the
// first fetch happens on the first assertion, so an outage never blocks start.
func New(cfg config.AuthJWT) (*Verifier, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	v := &Verifier{cfg: cfg, now: time.Now}
	if len(cfg.AllowedEmails) > 0 {
		v.emails = map[string]bool{}
		for _, e := range cfg.AllowedEmails {
			v.emails[asciiLower(strings.TrimSpace(e))] = true
		}
	}
	v.keys.init(cfg.JWKSURL, func() time.Time { return v.now() })
	return v, nil
}

// Verify checks the assertion in the configured header of r.
func (v *Verifier) Verify(r *http.Request) (Identity, error) {
	vals := r.Header.Values(v.cfg.Header)
	if len(vals) == 0 {
		return Identity{}, fail(ReasonMissing)
	}
	if len(vals) != 1 {
		return Identity{}, fail(ReasonMalformed)
	}
	parts := strings.Split(vals[0], ".")
	if len(parts) != 3 {
		return Identity{}, fail(ReasonMalformed)
	}
	var seg [3][]byte
	for i, p := range parts {
		b, ok := b64(p)
		if !ok {
			return Identity{}, fail(ReasonMalformed)
		}
		seg[i] = b
	}

	var hdr map[string]json.RawMessage
	if json.Unmarshal(seg[0], &hdr) != nil || hdr == nil {
		return Identity{}, fail(ReasonMalformed)
	}
	if _, ok := hdr["crit"]; ok {
		return Identity{}, fail(ReasonMalformed)
	}
	// jwk, jku, x5u and x5c are never read.
	var alg, kid string
	if json.Unmarshal(hdr["alg"], &alg) != nil || (alg != "RS256" && alg != "ES256") {
		return Identity{}, fail(ReasonAlg)
	}
	if json.Unmarshal(hdr["kid"], &kid) != nil || kid == "" {
		return Identity{}, fail(ReasonUnknownKid)
	}

	k, err := v.keys.get(kid)
	if err != nil {
		return Identity{}, err
	}
	if k.alg != alg { // the key decides; an HS256 or swapped alg dies here
		return Identity{}, fail(ReasonAlg)
	}
	// Over the received bytes "header.payload", never a re-encoding.
	signed := parts[0] + "." + parts[1]
	if !k.verify([]byte(signed), seg[2]) {
		return Identity{}, fail(ReasonSignature)
	}
	return v.claims(seg[1])
}

func (v *Verifier) claims(payload []byte) (Identity, error) {
	var c map[string]json.RawMessage
	if json.Unmarshal(payload, &c) != nil || c == nil {
		return Identity{}, fail(ReasonMalformed)
	}
	var iss, sub, email string
	if json.Unmarshal(c["iss"], &iss) != nil || iss == "" {
		return Identity{}, fail(ReasonClaims)
	}
	if iss != v.cfg.Issuer {
		return Identity{}, fail(ReasonIssuer)
	}
	var aud []string
	if json.Unmarshal(c["aud"], &aud) != nil {
		var one string
		if json.Unmarshal(c["aud"], &one) != nil {
			return Identity{}, fail(ReasonClaims)
		}
		aud = []string{one}
	}
	found := false
	for _, a := range aud {
		found = found || a == v.cfg.Audience
	}
	if !found {
		return Identity{}, fail(ReasonAudience)
	}
	exp, ok := numeric(c["exp"])
	if !ok {
		return Identity{}, fail(ReasonClaims)
	}
	now := v.now()
	if now.After(exp.Add(skew)) {
		return Identity{}, fail(ReasonExpired)
	}
	if raw, has := c["nbf"]; has {
		nbf, ok := numeric(raw)
		if !ok {
			return Identity{}, fail(ReasonClaims)
		}
		if now.Add(skew).Before(nbf) {
			return Identity{}, fail(ReasonNotYetValid)
		}
	}
	if json.Unmarshal(c["sub"], &sub) != nil || sub == "" {
		return Identity{}, fail(ReasonClaims)
	}
	if raw, has := c["email"]; has && json.Unmarshal(raw, &email) != nil {
		return Identity{}, fail(ReasonClaims)
	}
	// ASCII only: Unicode folding maps U+212A (Kelvin sign) to "k" and the like.
	for i := 0; i < len(email); i++ {
		if email[i] >= 0x80 {
			return Identity{}, fail(ReasonClaims)
		}
	}
	if v.emails != nil {
		if !v.emails[asciiLower(email)] {
			return Identity{}, fail(ReasonClaims)
		}
		// Some providers omit email_verified; an explicit false (bool or string) is refused.
		if raw, has := c["email_verified"]; has && string(raw) != "null" {
			var b bool
			var str string
			switch {
			case json.Unmarshal(raw, &b) == nil:
				if !b {
					return Identity{}, fail(ReasonClaims)
				}
			case json.Unmarshal(raw, &str) == nil:
				if str != "true" {
					return Identity{}, fail(ReasonClaims)
				}
			default:
				return Identity{}, fail(ReasonClaims)
			}
		}
	}
	return Identity{Subject: sub, Email: email}, nil
}

// numeric reads a NumericDate (seconds, possibly fractional).
func numeric(raw json.RawMessage) (time.Time, bool) {
	var f float64
	if string(raw) == "null" || json.Unmarshal(raw, &f) != nil || f < 0 || f > 1e12 {
		return time.Time{}, false
	}
	return time.Unix(0, int64(f*1e9)), true
}

// b64 decodes strict unpadded base64url: only the URL alphabet (Go's decoder
// would skip CR and LF), no padding, no stray bits.
func b64(s string) ([]byte, bool) {
	if s == "" {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return nil, false
		}
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return b, err == nil
}

// key is one usable public key; alg is fixed by its type.
type key struct {
	alg string
	rsa *rsa.PublicKey
	ec  *ecdsa.PublicKey
}

func (k key) verify(signed, sig []byte) bool {
	h := sha256.Sum256(signed)
	if k.rsa != nil {
		return rsa.VerifyPKCS1v15(k.rsa, crypto.SHA256, h[:], sig) == nil
	}
	if len(sig) != 64 { // r||s, checked before any maths
		return false
	}
	der, err := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])})
	return err == nil && ecdsa.VerifyASN1(k.ec, h[:], der)
}

// asciiLower lowers A-Z byte-wise and nothing else.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
