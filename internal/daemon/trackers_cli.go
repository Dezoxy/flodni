package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr"
)

// trackersLockTimeout is how long the trackers commands wait for config.db's file
// lock before they say the daemon is running; a test shortens it.
var trackersLockTimeout = 2 * time.Second

const trackersUsage = `usage: flodni trackers audit [--hashes] [--json] [--allow-root] [--state <dir>]
       flodni trackers remove-host [--dry-run] [--hashes] [--allow-same-site] [--allow-root] [--state <dir>] <host>
  --state is the engine's state directory (the daemon's --path; default: the binary's directory).
  Stop flodni first and copy config.db: both commands open it directly. Flags go before <host>.`

// trackersMain is `flodni trackers`: the owner-run, offline audit and cleanup of the
// trackers kept in the engine's stored records (config.db). Hard rule 3: it never
// prints a tracker URL, path, query or passkey, only hosts and info hashes. Exit 0
// on success, 1 when a record could not be written, 2 for a bad invocation, a
// database in use, a refusal, or a run as root without --allow-root.
func trackersMain(args []string, out, errOut io.Writer) int {
	usage := func(why string) int {
		_, _ = fmt.Fprintf(errOut, "trackers: %s\n%s\n", why, trackersUsage)
		return 2
	}
	if len(args) == 0 {
		return usage("a subcommand is required")
	}
	sub := args[0]
	if sub != "audit" && sub != "remove-host" {
		return usage("unknown subcommand")
	}
	fs := flag.NewFlagSet("trackers "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	state := fs.String("state", "", "state directory holding config.db")
	hashes := fs.Bool("hashes", false, "also list the info hashes")
	allowRoot := fs.Bool("allow-root", false, "run as root")
	var asJSON, dryRun, allowSameSite *bool
	if sub == "audit" {
		asJSON = fs.Bool("json", false, "machine-readable output")
	} else {
		dryRun = fs.Bool("dry-run", false, "print what would change, write nothing")
		allowSameSite = fs.Bool("allow-same-site", false, "also remove a host of the same site as a record's primary tracker")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return usage(err.Error())
	}
	var host string
	if sub == "remove-host" {
		// flag stops at the first positional, so a flag after the host would be ignored
		// and a dry run would write: exactly one argument, and it is the host.
		if f := misplacedFlag(fs.Args()); f != "" {
			return usage(fmt.Sprintf("usage error, nothing was changed: %s must come before <host>", f))
		}
		if fs.NArg() != 1 {
			return usage("usage error, nothing was changed: exactly one host is required, after the flags")
		}
		host = normHost(fs.Arg(0))
		if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, "/?@ ") {
			return usage("the argument must be a bare host name, flags go before it")
		}
		if strings.ContainsAny(host, "[]") {
			return usage("usage error, nothing was changed: the host must not carry brackets; pass an IPv6 address bare")
		}
		if strings.Contains(host, ":") && net.ParseIP(host) == nil {
			return usage("usage error, nothing was changed: the host must not carry a port; trackers match by host name only")
		}
	} else if fs.NArg() != 0 {
		return usage("usage error: audit takes no argument")
	}
	if scrubEuid() == 0 && !*allowRoot {
		return usage("refusing to run as root: run it as the daemon's user, or pass --allow-root")
	}
	dir := *state
	if dir == "" {
		exe, err := os.Executable()
		if err != nil {
			return usage("cannot find the state directory; pass --state")
		}
		dir = filepath.Dir(exe)
	}

	readOnly := sub == "audit" || *dryRun
	db, err := settings.OpenOffline(dir, trackersLockTimeout, readOnly)
	if err != nil {
		if errors.Is(err, settings.ErrDBInUse) {
			_, _ = fmt.Fprintln(errOut, "trackers: config.db is in use: stop flodni first")
		} else {
			_, _ = fmt.Fprintf(errOut, "trackers: %v (check --state)\n", err)
		}
		return 2
	}
	defer db.CloseDB()

	recs, unreadable := readTrackerRecords(db)
	if sub == "audit" {
		return trackersAudit(recs, unreadable, *hashes, *asJSON, out)
	}
	return trackersRemoveHost(db, recs, unreadable, host, *dryRun, *hashes, *allowSameSite, out, errOut)
}

// misplacedFlag returns the first flag-looking argument after the positional one. Go's
// flag package stops at the first positional, so such a flag would otherwise be
// ignored (a trailing --dry-run would not apply): it is refused by name instead.
func misplacedFlag(rest []string) string {
	for i, a := range rest {
		if i > 0 && strings.HasPrefix(a, "-") {
			if j := strings.IndexByte(a, '='); j > 0 {
				return a[:j]
			}
			return a
		}
	}
	return ""
}

// trackerRecord is one decodable stored record.
type trackerRecord struct {
	key     string
	hash    string
	private bool
	tiers   [][]string
}

// readTrackerRecords decodes every record the way LoadStored's read does (a JSON
// TorrentDB); a record that does not decode, or has no spec, is counted, not used.
func readTrackerRecords(db *settings.TDB) (recs []trackerRecord, unreadable int) {
	for _, key := range db.List("Torrents") {
		var rec *settings.TorrentDB
		if err := json.Unmarshal(db.Get("Torrents", key), &rec); err != nil || rec == nil || rec.TorrentSpec == nil {
			unreadable++
			continue
		}
		recs = append(recs, trackerRecord{
			key:     key,
			hash:    rec.InfoHash.HexString(),
			private: torr.InfoBytesPrivate(rec.InfoBytes),
			tiers:   rec.Trackers,
		})
	}
	return recs, unreadable
}

// normHost lower-cases a host and drops a trailing dot.
func normHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

// trackerHostOf is the normalised host of a tracker URL, or "" when it has none or
// holds a character no host name has (so nothing odd is ever printed).
func trackerHostOf(tr string) string {
	u, err := url.Parse(tr)
	if err != nil {
		return ""
	}
	h := normHost(u.Hostname())
	for _, c := range h {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c == ':') {
			return ""
		}
	}
	return h
}

// primaryHost is the host of the first tracker of the first tier (the first one
// with a host, should that tracker have none).
func primaryHost(tiers [][]string) string {
	for _, tier := range tiers {
		for _, tr := range tier {
			if h := trackerHostOf(tr); h != "" {
				return h
			}
		}
	}
	return ""
}

// twoLabelSuffixes is a small built-in list of public suffixes with two labels, so
// site.co.uk is a registrable domain and not "co.uk". It is an approximation, not
// the Public Suffix List: a suffix missing here makes its hosts look like one
// domain, so a foreign host under it can be missed (never the reverse).
var twoLabelSuffixes = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true, "me.uk": true, "ltd.uk": true, "net.uk": true,
	"com.hu": true, "co.hu": true, "org.hu": true, "net.hu": true,
	"com.au": true, "net.au": true, "org.au": true,
	"co.nz": true, "co.jp": true, "co.za": true, "co.il": true, "co.in": true,
	"com.br": true, "com.mx": true, "com.ar": true, "com.tr": true, "com.pl": true, "com.ua": true, "com.cn": true,
}

// ownDomain is the last two labels of a host (three under a listed two-label
// suffix). An IP literal or a single label is its own domain.
func ownDomain(host string) string {
	if strings.Contains(host, ":") || net.ParseIP(host) != nil {
		return host
	}
	labels := strings.Split(host, ".")
	n := len(labels)
	if n <= 2 {
		return host
	}
	if twoLabelSuffixes[strings.Join(labels[n-2:], ".")] {
		return strings.Join(labels[n-3:], ".")
	}
	return strings.Join(labels[n-2:], ".")
}

// siteName is the first label of a host's registrable domain: tracker.example.com and
// example.org are both "example". A tracker often serves the same torrents from a
// second domain of its own site on another top-level domain, so hosts are grouped by
// it. An IP literal or a single label is its own name.
func siteName(host string) string {
	d := ownDomain(host)
	if strings.Contains(d, ":") || net.ParseIP(d) != nil {
		return d
	}
	if i := strings.IndexByte(d, '.'); i > 0 {
		return d[:i]
	}
	return d
}

// otherHosts sorts a record's hosts outside its primary host's registrable domain into
// foreign ones (another site) and same-site ones (another domain whose site name, the
// first label of the registrable domain, is the primary's: often the tracker's own
// second top-level domain, but a lookalike would read the same, so they are reported
// apart for the owner to check, never hidden).
func otherHosts(r trackerRecord) (foreign, sameSite map[string]bool) {
	p := primaryHost(r.tiers)
	if p == "" {
		return nil, nil
	}
	ownD, ownS := ownDomain(p), siteName(p)
	for _, tier := range r.tiers {
		for _, tr := range tier {
			h := trackerHostOf(tr)
			if h == "" || ownDomain(h) == ownD {
				continue
			}
			if siteName(h) == ownS {
				if sameSite == nil {
					sameSite = map[string]bool{}
				}
				sameSite[h] = true
				continue
			}
			if foreign == nil {
				foreign = map[string]bool{}
			}
			foreign[h] = true
		}
	}
	return foreign, sameSite
}

type auditHost struct {
	Host     string   `json:"host"`
	Records  int      `json:"records"`
	Private  int      `json:"private"`
	SameSite bool     `json:"same_site"`
	Hashes   []string `json:"hashes,omitempty"`
}

type auditReport struct {
	Hosts           []auditHost `json:"hosts"`
	Primaries       []auditSite `json:"primaries"`
	Scanned         int         `json:"scanned"`
	Private         int         `json:"private"`
	Public          int         `json:"public"`
	ForeignRecords  int         `json:"foreign_records"`
	SameSiteRecords int         `json:"same_site_records"`
	Unreadable      int         `json:"unreadable,omitempty"`
	// UnparsedTrackers counts tracker entries whose host could not be parsed; NoPrimary
	// counts records with no parsable host at all (the audit cannot judge those).
	UnparsedTrackers int `json:"unparsed_trackers"`
	NoPrimary        int `json:"no_primary"`
	// Endpoints lists scheme://host:port of the tracker URLs, nothing else of them.
	Endpoints auditEndpoints `json:"endpoints"`
}

// auditEndpoints splits the endpoints by the records' privacy. Unparsed counts tracker
// entries that gave no endpoint; they are never printed.
type auditEndpoints struct {
	Private  []auditEndpoint `json:"private"`
	Public   []auditEndpoint `json:"public"`
	Unparsed int             `json:"unparsed"`
}

// auditEndpoint is one endpoint and how many records list it (once per record).
type auditEndpoint struct {
	Endpoint string `json:"endpoint"`
	Records  int    `json:"records"`
}

// trackerEndpointOf is scheme://host:port of a tracker URL with the default port made
// explicit (http 80, https 443; udp must carry its port), or "" when the URL does not
// parse, has another scheme, no valid host or no port. The path, query, fragment and
// userinfo are dropped before anything is built, so no passkey can reach the output.
func trackerEndpointOf(tr string) string {
	u, err := url.Parse(strings.TrimSpace(tr))
	if err != nil {
		return ""
	}
	h := trackerHostOf(tr)
	if h == "" {
		return ""
	}
	port := u.Port()
	switch u.Scheme {
	case "http", "https", "ws", "wss":
		if port == "" {
			port = "80"
			if u.Scheme == "https" || u.Scheme == "wss" {
				port = "443"
			}
		}
	case "udp", "udp4", "udp6":
		if port == "" {
			return ""
		}
	default:
		return ""
	}
	return u.Scheme + "://" + net.JoinHostPort(h, port)
}

// auditSite is one primary site (the registrable domain of a record's first tracker)
// and how many records lead with it.
type auditSite struct {
	Site    string `json:"site"`
	Records int    `json:"records"`
}

func trackersAudit(recs []trackerRecord, unreadable int, hashes, asJSON bool, out io.Writer) int {
	rep := auditReport{Hosts: []auditHost{}, Primaries: []auditSite{}, Scanned: len(recs), Unreadable: unreadable}
	// A host can be same-site for one record and foreign for another: key by both.
	type hostClass struct {
		host     string
		sameSite bool
	}
	byHost := map[hostClass]*auditHost{}
	bySite := map[string]int{}
	epPrivate, epPublic := map[string]int{}, map[string]int{}
	for _, r := range recs {
		if r.private {
			rep.Private++
		} else {
			rep.Public++
		}
		seenEp := map[string]bool{}
		for _, tier := range r.tiers {
			for _, tr := range tier {
				if trackerHostOf(tr) == "" {
					rep.UnparsedTrackers++
				}
				ep := trackerEndpointOf(tr)
				if ep == "" {
					rep.Endpoints.Unparsed++
					continue
				}
				if seenEp[ep] {
					continue
				}
				seenEp[ep] = true
				if r.private {
					epPrivate[ep]++
				} else {
					epPublic[ep]++
				}
			}
		}
		if p := primaryHost(r.tiers); p == "" {
			rep.NoPrimary++
		} else {
			bySite[ownDomain(p)]++
		}
		f, same := otherHosts(r)
		if len(f) > 0 {
			rep.ForeignRecords++
		}
		if len(same) > 0 {
			rep.SameSiteRecords++
		}
		add := func(h string, sameSite bool) {
			e := byHost[hostClass{h, sameSite}]
			if e == nil {
				e = &auditHost{Host: h, SameSite: sameSite}
				byHost[hostClass{h, sameSite}] = e
			}
			e.Records++
			if r.private {
				e.Private++
			}
			if hashes {
				e.Hashes = append(e.Hashes, r.hash)
			}
		}
		for h := range f {
			add(h, false)
		}
		for h := range same {
			add(h, true)
		}
	}
	for _, e := range byHost {
		sort.Strings(e.Hashes)
		rep.Hosts = append(rep.Hosts, *e)
	}
	sort.Slice(rep.Hosts, func(i, j int) bool {
		if rep.Hosts[i].Records != rep.Hosts[j].Records {
			return rep.Hosts[i].Records > rep.Hosts[j].Records
		}
		if rep.Hosts[i].Host != rep.Hosts[j].Host {
			return rep.Hosts[i].Host < rep.Hosts[j].Host
		}
		return !rep.Hosts[i].SameSite && rep.Hosts[j].SameSite
	})
	for site, n := range bySite {
		rep.Primaries = append(rep.Primaries, auditSite{site, n})
	}
	sort.Slice(rep.Primaries, func(i, j int) bool {
		if rep.Primaries[i].Records != rep.Primaries[j].Records {
			return rep.Primaries[i].Records > rep.Primaries[j].Records
		}
		return rep.Primaries[i].Site < rep.Primaries[j].Site
	})
	rep.Endpoints.Private, rep.Endpoints.Public = sortedEndpoints(epPrivate), sortedEndpoints(epPublic)
	if asJSON {
		enc := json.NewEncoder(out)
		if err := enc.Encode(rep); err != nil {
			return 1
		}
		return 0
	}
	print := func(sameSite bool) {
		for _, e := range rep.Hosts {
			if e.SameSite != sameSite {
				continue
			}
			_, _ = fmt.Fprintf(out, "%s  records=%d  private=%d\n", e.Host, e.Records, e.Private)
			for _, h := range e.Hashes {
				_, _ = fmt.Fprintf(out, "  %s\n", h)
			}
		}
	}
	print(false)
	hasSame := false
	for _, e := range rep.Hosts {
		hasSame = hasSame || e.SameSite
	}
	if hasSame {
		_, _ = fmt.Fprintln(out, "same site, other domain (verify: often the tracker's own second domain, a lookalike reads the same; remove-host needs --allow-same-site):")
		print(true)
	}
	_, _ = fmt.Fprintln(out, "primaries (the first tracker of each record, by site; check that it is your own):")
	for _, ps := range rep.Primaries {
		_, _ = fmt.Fprintf(out, "%s  records=%d\n", ps.Site, ps.Records)
	}
	_, _ = fmt.Fprintln(out, "endpoints (scheme://host:port, default ports explicit; nothing of the URL's path, query or userinfo):")
	for _, g := range []struct {
		name string
		eps  []auditEndpoint
	}{{"private", rep.Endpoints.Private}, {"public", rep.Endpoints.Public}} {
		_, _ = fmt.Fprintf(out, " %s records:\n", g.name)
		for _, e := range g.eps {
			_, _ = fmt.Fprintf(out, "%s  records=%d\n", e.Endpoint, e.Records)
		}
	}
	_, _ = fmt.Fprintf(out, "endpoints unparsed=%d\n", rep.Endpoints.Unparsed)
	_, _ = fmt.Fprintf(out, "scanned=%d private=%d public=%d foreign_records=%d same_site_records=%d unparsed_trackers=%d no_primary=%d%s\n",
		rep.Scanned, rep.Private, rep.Public, rep.ForeignRecords, rep.SameSiteRecords, rep.UnparsedTrackers, rep.NoPrimary, unreadableNote(unreadable))
	return 0
}

// sortedEndpoints lists the endpoints sorted by name, never nil.
func sortedEndpoints(m map[string]int) []auditEndpoint {
	out := make([]auditEndpoint, 0, len(m))
	for ep, n := range m {
		out = append(out, auditEndpoint{ep, n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out
}

func unreadableNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" unreadable=%d", n)
}

// withoutHost drops every tracker of the host, empty tiers with it, and keeps the
// order of the rest. It reports how many trackers it dropped.
func withoutHost(tiers [][]string, host string) (out [][]string, removed int) {
	for _, tier := range tiers {
		var kept []string
		for _, tr := range tier {
			if trackerHostOf(tr) == host {
				removed++
				continue
			}
			kept = append(kept, tr)
		}
		if len(kept) > 0 {
			out = append(out, kept)
		}
	}
	return out, removed
}

// rewriteTrackers returns the stored JSON with only its Trackers value replaced:
// every other field keeps its bytes.
func rewriteTrackers(raw []byte, tiers [][]string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	t, err := json.Marshal(tiers)
	if err != nil {
		return nil, err
	}
	m["Trackers"] = t
	return json.Marshal(m)
}

// trackersRewrite is rewriteTrackers; a test replaces it to prove the check below.
var trackersRewrite = rewriteTrackers

// verifyRewrite checks the bytes about to be written: they decode, their Trackers are
// the planned tiers, and every other top-level key is present with identical raw bytes.
func verifyRewrite(orig, buf []byte, tiers [][]string) error {
	var after struct{ Trackers [][]string }
	if err := json.Unmarshal(buf, &after); err != nil {
		return err
	}
	if !reflect.DeepEqual(after.Trackers, tiers) {
		return errors.New("trackers differ from the plan")
	}
	var o, n map[string]json.RawMessage
	if err := json.Unmarshal(orig, &o); err != nil {
		return err
	}
	if err := json.Unmarshal(buf, &n); err != nil {
		return err
	}
	delete(o, "Trackers")
	delete(n, "Trackers")
	if len(o) != len(n) {
		return errors.New("key set changed")
	}
	for k, v := range o {
		if nv, ok := n[k]; !ok || !bytes.Equal(v, nv) {
			return errors.New("a field changed")
		}
	}
	return nil
}

func trackersRemoveHost(db *settings.TDB, recs []trackerRecord, unreadable int, host string, dryRun, hashes, allowSameSite bool, out, errOut io.Writer) int {
	type change struct {
		rec   trackerRecord
		tiers [][]string
	}
	var changes []change
	removed, priv, pub := 0, 0, 0
	var primaryHashes, sameSiteHashes []string
	for _, r := range recs {
		tiers, n := withoutHost(r.tiers, host)
		if n == 0 {
			continue
		}
		// The primary's own registrable domain is never removable; another domain of the
		// same site only with --allow-same-site (it is usually the tracker's own).
		if p := primaryHost(r.tiers); p != "" {
			switch {
			case p == host || ownDomain(p) == ownDomain(host):
				primaryHashes = append(primaryHashes, r.hash)
			case siteName(p) == siteName(host):
				sameSiteHashes = append(sameSiteHashes, r.hash)
			}
		}
		removed += n
		if r.private {
			priv++
		} else {
			pub++
		}
		changes = append(changes, change{r, tiers})
	}
	listHashes := func(hs []string) {
		if hashes {
			for _, h := range hs {
				_, _ = fmt.Fprintf(out, "  %s\n", h)
			}
		}
	}
	if len(primaryHashes) > 0 {
		listHashes(primaryHashes)
		_, _ = fmt.Fprintf(errOut, "trackers: refusing: that host is in the primary tracker's own domain for %d record(s); nothing was changed\n", len(primaryHashes))
		return 2
	}
	if len(sameSiteHashes) > 0 && !allowSameSite {
		listHashes(sameSiteHashes)
		_, _ = fmt.Fprintf(errOut, "trackers: refusing: that host shares the site name of the primary tracker of %d record(s), so it is likely the tracker's own second domain; check it, then pass --allow-same-site to remove it; nothing was changed\n", len(sameSiteHashes))
		return 2
	}
	if hashes {
		for _, c := range changes {
			_, _ = fmt.Fprintf(out, "  %s\n", c.rec.hash)
		}
	}
	if !dryRun {
		for _, c := range changes {
			raw := db.Get("Torrents", c.rec.key)
			buf, err := trackersRewrite(raw, c.tiers)
			if err == nil {
				err = verifyRewrite(raw, buf, c.tiers)
			}
			if err == nil {
				err = db.Put("Torrents", c.rec.key, buf)
			}
			if err != nil {
				// Never the error text of a record: say which hash, nothing more.
				_, _ = fmt.Fprintf(errOut, "trackers: could not rewrite record %s; stopped\n", c.rec.hash)
				return 1
			}
		}
	}
	if len(changes) == 0 {
		_, _ = fmt.Fprintln(out, "no record has that host")
	}
	note := ""
	if dryRun {
		note = " (dry run: nothing written)"
	}
	_, _ = fmt.Fprintf(out, "records_changed=%d trackers_removed=%d private=%d public=%d%s%s\n",
		len(changes), removed, priv, pub, unreadableNote(unreadable), note)
	return 0
}
