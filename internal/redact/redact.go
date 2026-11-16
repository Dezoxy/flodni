// Package redact keeps tracker credentials out of logs. A private tracker's
// announce URL is the user's account: the passkey sits in its path, its query
// or its userinfo. Logs are shared when asking for help, so every log line that
// can carry a link, a magnet or an error built from one goes through String
// (or Err) first.
//
// What survives is scheme://host[:port] and a fixed placeholder, so a line still
// says which tracker failed and how. Callers pass whole strings, not parsed URLs:
// errors from net/http embed the full URL, and the redactor finds it in the text.
package redact

import (
	"net/url"
	"regexp"
	"strings"
)

// placeholder stands for everything after the host of a tracker URL.
const placeholder = "/<redacted>"

// reToken finds what can hold a tracker URL: a magnet link (matched whole, so its tr=
// and xs= values are handled as parameters), a bare http(s)/udp/ws(s) URL, and a
// torrs:// link, which packs the torrent's trackers into an opaque token. A token
// ends at white space, a quote (also an escaped one in a %q or slog-quoted line) or an angle bracket.
var reToken = regexp.MustCompile(`(?i)magnet:\?[^\s"'<>\\]+|\b(?:https?|udp|wss?|torrs)://[^\s"'<>\\]+`)

// reURL tells a decoded magnet value that is itself a URL.
var reURL = regexp.MustCompile(`(?i)^(?:https?|udp|wss?)://`)

// reKey is the last net for a credential outside any URL: a stray "passkey=...".
var reKey = regexp.MustCompile(`(?i)((?:pk|passkey|authkey|torrent_pass|uk|apikey)(?:=|%3D))[^&%\s"]+`)

// reQueryToken masks the value of a token (or access_token) query parameter wherever it
// shows: a request path in a log line, a quoted line, or text around a URL. The
// value ends at the next parameter, white space, a quote or an angle bracket.
var reQueryToken = regexp.MustCompile(`(?i)\b((?:access_)?token(?:=|%3D))[^&\s"'<>\\]+`)

// reAuthHeader finds an Authorization header line; the scheme (Bearer, Basic) stays.
var reAuthHeader = regexp.MustCompile(`(?i)\b(authorization\s*:\s*)(?:(bearer|basic)\s+)?[^\s"'<>\\][^\r\n"'<>\\]*`)

// reCookieHeader finds a Cookie or Set-Cookie header line.
var reCookieHeader = regexp.MustCompile(`(?i)\b((?:set-)?cookie\s*:\s*)[^\s"'<>\\][^\r\n"'<>\\]*`)

// String masks every tracker credential in s: the userinfo, path and query of each
// URL, the tracker and exact-source values of each magnet, stray key=value
// credentials, the value of every token= query parameter and the value of an
// Authorization, Cookie or Set-Cookie header line. Text with none of these comes back unchanged.
func String(s string) string {
	s = reToken.ReplaceAllStringFunc(s, func(tok string) string {
		if strings.HasPrefix(strings.ToLower(tok), "magnet:") {
			return magnet(tok)
		}
		return urlToken(tok)
	})
	s = reKey.ReplaceAllString(s, "${1}***")
	s = reQueryToken.ReplaceAllString(s, "${1}<redacted>")
	s = reAuthHeader.ReplaceAllStringFunc(s, func(m string) string {
		sub := reAuthHeader.FindStringSubmatch(m)
		if sub[2] != "" {
			return sub[1] + sub[2] + " <redacted>"
		}
		return sub[1] + "<redacted>"
	})
	return reCookieHeader.ReplaceAllString(s, "${1}<redacted>")
}

// Err is String of err's text; a nil error gives "".
func Err(err error) string {
	if err == nil {
		return ""
	}
	return String(err.Error())
}

// urlToken reduces one URL to scheme://host[:port], plus the placeholder when
// there was anything after the host. Trailing punctuation that belongs to the
// sentence around the URL stays outside.
func urlToken(tok string) string {
	trimmed := strings.TrimRight(tok, ".,;:)]}")
	tail := tok[len(trimmed):]
	scheme, rest, _ := strings.Cut(trimmed, "://")
	if strings.EqualFold(scheme, "torrs") {
		return scheme + "://<redacted>" + tail
	}
	authority, after := rest, ""
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority, after = rest[:i], rest[i:]
	}
	hidden := after != "" && after != "/"
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority, hidden = authority[i+1:], true
	}
	out := scheme + "://" + authority
	switch {
	case hidden:
		out += placeholder
	case after == "/":
		out += "/"
	}
	return out + tail
}

// magnet keeps a magnet's hash and name and reduces its tr= and xs= values (the
// tracker URLs and the exact source), which is where a passkey sits.
func magnet(tok string) string {
	head, query, _ := strings.Cut(tok, "?")
	params := strings.Split(query, "&")
	for i, p := range params {
		k, v, ok := strings.Cut(p, "=")
		if !ok || !isSourceKey(k) {
			continue
		}
		params[i] = k + "=" + sourceValue(v)
	}
	return head + "?" + strings.Join(params, "&")
}

// isSourceKey matches tr, xs and their numbered forms (tr.1, xs.2).
func isSourceKey(k string) bool {
	k = strings.ToLower(k)
	base, _, _ := strings.Cut(k, ".")
	return base == "tr" || base == "xs"
}

// sourceValue shows the host of a tracker or source URL and hides the rest; a
// value that is no URL is dropped whole.
func sourceValue(v string) string {
	dec, err := url.QueryUnescape(v)
	if err != nil {
		dec = v
	}
	if reURL.MatchString(dec) {
		return urlToken(dec)
	}
	return "<redacted>"
}
