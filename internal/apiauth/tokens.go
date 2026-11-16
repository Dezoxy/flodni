package apiauth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
)

// This file is the token store of ADR 0004: the scopes, the tokens file and its
// loader, and Lookup. It enforces nothing; the gate that consumes it is a later step.

// Scope is one capability a token can hold.
type Scope string

const (
	ScopeWebhook Scope = "webhook"
	ScopeMetrics Scope = "metrics"
	ScopeRead    Scope = "read"
	ScopeLibrary Scope = "library"
	ScopeScrape  Scope = "scrape"
	ScopeEngine  Scope = "engine"
	ScopeUpload  Scope = "upload"
	ScopeAdmin   Scope = "admin"
)

// AllScopes is every scope, sorted by name.
var AllScopes = []Scope{ScopeAdmin, ScopeEngine, ScopeLibrary, ScopeMetrics, ScopeRead, ScopeScrape, ScopeUpload, ScopeWebhook}

// ScopeSet is a set of scopes.
type ScopeSet uint16

func scopeBit(s Scope) ScopeSet {
	for i, a := range AllScopes {
		if a == s {
			return 1 << i
		}
	}
	return 0
}

func scopeSetOf(scopes ...Scope) ScopeSet {
	var set ScopeSet
	for _, s := range scopes {
		set |= scopeBit(s)
	}
	return set
}

// Has reports whether the set grants s. admin implies every other scope; no other
// scope implies another.
func (s ScopeSet) Has(sc Scope) bool {
	bit := scopeBit(sc)
	return bit != 0 && s&(bit|scopeBit(ScopeAdmin)) != 0
}

// Names lists the scopes the set holds as written, sorted, without expanding admin.
func (s ScopeSet) Names() []string {
	out := []string{}
	for _, a := range AllScopes {
		if s&scopeBit(a) != 0 {
			out = append(out, string(a))
		}
	}
	return out
}

// ParseScopes reads a comma-separated scope list. Every item must be a known scope
// and the list must not be empty.
func ParseScopes(list string) (ScopeSet, error) {
	names := strings.Split(list, ",")
	for i := range names {
		names[i] = strings.TrimSpace(names[i]) // the command line only; the file is exact
	}
	return scopesFromNames(names)
}

func scopesFromNames(names []string) (ScopeSet, error) {
	var set ScopeSet
	for _, n := range names {
		bit := scopeBit(Scope(n))
		if bit == 0 {
			return 0, fmt.Errorf("unknown scope %q (known: %s)", n, knownScopes())
		}
		set |= bit
	}
	if set == 0 {
		return 0, errors.New("no scopes")
	}
	return set, nil
}

func knownScopes() string {
	names := make([]string, len(AllScopes))
	for i, a := range AllScopes {
		names[i] = string(a)
	}
	return strings.Join(names, ", ")
}

// Mode is how the two servers are protected.
type Mode string

const (
	ModeOpen        Mode = "open"         // no credential configured
	ModeLegacyToken Mode = "legacy_token" // api_token alone: one token, every scope
	ModeScoped      Mode = "scoped"       // a tokens file
)

const (
	maxFileBytes  = 64 << 10
	maxTokens     = 64
	reservedName  = "internal"
	legacyName    = "legacy"
	tokenPrefix   = "fl_"
	tokenBodyLen  = 43 // 32 bytes, base64url without padding
	tokenShapeLen = len(tokenPrefix) + tokenBodyLen
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidName reports whether name may label a token.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("name %q must match [a-z0-9][a-z0-9_-]{0,31}", name)
	}
	if name == reservedName {
		return fmt.Errorf("name %q is reserved", name)
	}
	return nil
}

// Entry is one token as held in memory.
type Entry struct {
	Name   string
	Scopes ScopeSet
	hash   [sha256.Size]byte
}

// Tokens is the result of Load: the mode and the entries the gate looks tokens up in.
// It is immutable after Load. A nil *Tokens reads as open mode.
type Tokens struct {
	mode    Mode
	entries []Entry
	jwt     bool
	// cmp compares two hashes; a test replaces it to count the comparisons.
	cmp func(a, b []byte) int
}

func constantTimeCompare(a, b []byte) int { return subtle.ConstantTimeCompare(a, b) }

// Mode reports how the servers are protected.
func (t *Tokens) Mode() Mode {
	if t == nil {
		return ModeOpen
	}
	return t.mode
}

// Len is the number of entries: 0 open, 1 legacy_token, the file's count scoped.
func (t *Tokens) Len() int {
	if t == nil {
		return 0
	}
	return len(t.entries)
}

// validShape reports whether s is "fl_" plus 43 base64url characters.
func validShape(s string) bool {
	if len(s) != tokenShapeLen || !strings.HasPrefix(s, tokenPrefix) {
		return false
	}
	for i := len(tokenPrefix); i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Lookup finds the entry a presented token belongs to. In scoped mode a value that
// is not "fl_" plus 43 base64url characters never matches and is not hashed. The
// presented token is hashed once and compared with every stored hash in constant
// time, with no early exit, so the work does not depend on which entry matched. In
// legacy_token mode the one entry is the api_token, exempt from the shape rule
// because the owner chose it. Open mode has nothing to find.
func (t *Tokens) Lookup(presented string) (name string, scopes ScopeSet, ok bool) {
	if t == nil || len(t.entries) == 0 {
		return "", 0, false
	}
	if t.mode == ModeScoped && !validShape(presented) {
		return "", 0, false
	}
	cmp := t.cmp
	if cmp == nil {
		cmp = constantTimeCompare
	}
	h := sha256.Sum256([]byte(presented))
	idx, found := 0, 0
	for i := range t.entries {
		eq := cmp(h[:], t.entries[i].hash[:])
		idx = subtle.ConstantTimeSelect(eq, i, idx)
		found |= eq
	}
	if found != 1 {
		return "", 0, false
	}
	return t.entries[idx].Name, t.entries[idx].Scopes, true
}

// Report is the auth section of the startup report. It holds no name, hash, path
// or key.
type Report struct {
	Mode   string   `json:"mode"`
	Tokens int      `json:"tokens"`
	Scopes []string `json:"scopes"`
	JWT    bool     `json:"jwt"`
}

// Report summarises the mode for the startup report: the entry count and the sorted
// union of the scopes as written (admin is not expanded). Never nil Scopes.
func (t *Tokens) Report() Report {
	r := Report{Mode: string(t.Mode()), Tokens: t.Len(), Scopes: []string{}}
	if t == nil {
		return r
	}
	var union ScopeSet
	for _, e := range t.entries {
		union |= e.Scopes
	}
	r.Scopes = union.Names()
	r.JWT = t.jwt
	return r
}

// LoadOptions is what Load needs from the configuration.
type LoadOptions struct {
	APIToken   string // api_token, after the environment override
	TokensFile string // auth_tokens_file, after the environment override
	GoStormURL string // gostorm_url, checked in scoped mode
	// JWTConfigured is true when an auth_jwt block is set. The block itself is parsed
	// by a later step (ADR 0004 step 5); until the daemon passes the real value the
	// refusal below (auth_jwt without a tokens file) cannot fire in a running daemon,
	// and only the tests exercise it.
	JWTConfigured bool
	Logger        *slog.Logger // for the one warning; nil logs nothing
}

// Load decides the mode from the configuration and, for a tokens file, reads it once.
// Any error is a refusal to start: it never falls back to open mode.
func Load(o LoadOptions) (*Tokens, error) {
	if o.APIToken != "" && o.TokensFile != "" {
		return nil, errors.New("api_token and auth_tokens_file are both set: remove api_token (there is no alias; the scoped tokens replace it)")
	}
	if o.JWTConfigured && o.TokensFile == "" {
		return nil, errors.New("auth_jwt is set without auth_tokens_file: the proxy sign-in needs scoped mode")
	}
	if o.TokensFile == "" {
		if o.APIToken == "" {
			return &Tokens{mode: ModeOpen}, nil
		}
		return &Tokens{mode: ModeLegacyToken, entries: []Entry{{
			Name: legacyName, Scopes: scopeSetOf(AllScopes...), hash: sha256.Sum256([]byte(o.APIToken)),
		}}}, nil
	}
	if err := requireLoopback(o.GoStormURL); err != nil {
		return nil, err
	}
	entries, err := readTokensFile(o.TokensFile, o.Logger)
	if err != nil {
		return nil, fmt.Errorf("auth_tokens_file %s: %w", o.TokensFile, err)
	}
	return &Tokens{mode: ModeScoped, entries: entries, jwt: o.JWTConfigured}, nil
}

// requireLoopback refuses a gostorm_url that is not provably this machine: the engine
// is called without a credential, so in scoped mode it must not leave the host.
func requireLoopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("gostorm_url does not parse: %w", err)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("gostorm_url must be a loopback address (127.0.0.0/8, ::1 or localhost) when auth_tokens_file is set")
}

// FileEntry is one token as written in the file.
type FileEntry struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	SHA256 string   `json:"sha256"`
}

type tokensFile struct {
	Tokens []FileEntry `json:"tokens"`
}

func readTokensFile(path string, logger *slog.Logger) ([]Entry, error) {
	// O_NONBLOCK: opening a FIFO with no writer must not block the start forever.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("is not a regular file")
	}
	if fi.Mode().Perm()&0o022 != 0 && logger != nil {
		logger.Warn("[Auth] the tokens file is writable by group or others", "mode", fi.Mode().Perm().String())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFileBytes {
		return nil, errors.New("larger than 64 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var file tokensFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse: %w%s", err, lineOf(data, err))
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("parse: trailing data after the JSON object")
	}
	return validateEntries(file.Tokens)
}

// rejectDuplicateKeys refuses an object that repeats a key. encoding/json keeps the last
// one and matches keys without regard to case, so {"scopes":["webhook"],"scopes":["admin"]}
// would load as admin; the keys are compared folded to lower case for the same reason.
// It scans the first JSON value only, and the data has already decoded cleanly.
func rejectDuplicateKeys(data []byte) error {
	type frame struct {
		obj    bool
		wantKy bool
		seen   map[string]bool
	}
	var stack []*frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].obj {
			stack[n-1].wantKy = true
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil // the decode above reported anything malformed
		}
		d, isDelim := tok.(json.Delim)
		if n := len(stack); n > 0 && stack[n-1].obj && stack[n-1].wantKy && !isDelim {
			key, _ := tok.(string)
			key = strings.ToLower(key)
			if stack[n-1].seen[key] {
				return fmt.Errorf("parse: duplicate key %q", key)
			}
			stack[n-1].seen[key] = true
			stack[n-1].wantKy = false
			continue
		}
		if isDelim {
			switch d {
			case '{':
				stack = append(stack, &frame{obj: true, wantKy: true, seen: map[string]bool{}})
			case '[':
				stack = append(stack, &frame{})
			default:
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return nil
				}
				valueDone()
			}
			continue
		}
		if len(stack) == 0 {
			return nil
		}
		valueDone()
	}
}

// lineOf names the line of a syntax error, so a mistake in the file can be found.
func lineOf(data []byte, err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		off := int(se.Offset)
		if off > len(data) {
			off = len(data)
		}
		return fmt.Sprintf(" (line %d)", 1+bytes.Count(data[:off], []byte{'\n'}))
	}
	return ""
}

var emptyHash = sha256.Sum256(nil)

func validateEntries(in []FileEntry) ([]Entry, error) {
	if len(in) == 0 {
		return nil, errors.New("holds no tokens")
	}
	if len(in) > maxTokens {
		return nil, fmt.Errorf("holds %d tokens, more than %d", len(in), maxTokens)
	}
	names, hashes := map[string]bool{}, map[string]bool{}
	out := make([]Entry, 0, len(in))
	for i, e := range in {
		if err := ValidName(e.Name); err != nil {
			return nil, fmt.Errorf("token %d: %w", i+1, err)
		}
		if names[e.Name] {
			return nil, fmt.Errorf("token %d: name %q repeats", i+1, e.Name)
		}
		names[e.Name] = true
		scopes, err := scopesFromNames(e.Scopes)
		if err != nil {
			return nil, fmt.Errorf("token %q: %w", e.Name, err)
		}
		if !hashRE.MatchString(e.SHA256) {
			return nil, fmt.Errorf("token %q: sha256 must be 64 lower-case hex characters", e.Name)
		}
		var h [sha256.Size]byte
		if _, err := hex.Decode(h[:], []byte(e.SHA256)); err != nil {
			return nil, fmt.Errorf("token %q: sha256: %w", e.Name, err)
		}
		if h == emptyHash {
			return nil, fmt.Errorf("token %q: sha256 is the hash of the empty string", e.Name)
		}
		if hashes[e.SHA256] {
			return nil, fmt.Errorf("token %q: sha256 repeats another entry", e.Name)
		}
		hashes[e.SHA256] = true
		out = append(out, Entry{Name: e.Name, Scopes: scopes, hash: h})
	}
	return out, nil
}

// GenerateToken makes a token: "fl_" plus 32 bytes from crypto/rand in base64url, and
// the hex SHA-256 the file holds for it.
func GenerateToken() (token, sha256Hex string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	h := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(h[:]), nil
}
