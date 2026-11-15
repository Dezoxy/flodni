package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"flodni/internal/gostorm/settings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	bolt "go.etcd.io/bbolt"
)

const trkPasskey = "0123456789abcdef0123456789abcdef"

func trkHash(n int) metainfo.Hash {
	var h metainfo.Hash
	copy(h[:], fmt.Sprintf("%020d", n))
	return h
}

func trkInfo(t *testing.T, private bool) []byte {
	t.Helper()
	info := metainfo.Info{Name: "x", PieceLength: 16384, Length: 1, Pieces: make([]byte, 20)}
	if private {
		p := true
		info.Private = &p
	}
	b, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// trkRecord is one stored record: its tracker tiers and whether the metainfo is private.
type trkRecord struct {
	n       int
	private bool
	tiers   [][]string
}

// trkState builds <dir>/config.db through the settings write path (TDB.Put), with the
// record JSON the engine writes (a TorrentDB), and returns the state directory.
func trkState(t *testing.T, recs ...trkRecord) string {
	t.Helper()
	dir := t.TempDir()
	raw, err := bolt.Open(filepath.Join(dir, "config.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := settings.OpenOffline(dir, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.CloseDB()
	for _, r := range recs {
		h := trkHash(r.n)
		buf, err := json.Marshal(&settings.TorrentDB{
			TorrentSpec: &torrent.TorrentSpec{InfoHash: h, InfoBytes: trkInfo(t, r.private), Trackers: r.tiers},
			Title:       "t", Category: "movie", Size: 7, Timestamp: int64(r.n),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Put("Torrents", h.HexString(), buf); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func trkTiers(t *testing.T, dir string, n int) [][]string {
	t.Helper()
	db, err := settings.OpenOffline(dir, time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.CloseDB()
	var rec settings.TorrentDB
	if err := json.Unmarshal(db.Get("Torrents", trkHash(n).HexString()), &rec); err != nil {
		t.Fatal(err)
	}
	return rec.Trackers
}

func runTrk(args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	old := scrubEuid
	scrubEuid = func() int { return 1000 }
	defer func() { scrubEuid = old }()
	code = trackersMain(args, &o, &e)
	return code, o.String(), e.String()
}

func noLeak(t *testing.T, outs ...string) {
	t.Helper()
	for _, s := range outs {
		for _, bad := range []string{"://", "announce", trkPasskey, "passkey", "?"} {
			if strings.Contains(s, bad) {
				t.Errorf("output leaks %q: %q", bad, s)
			}
		}
	}
}

func trkFixture(t *testing.T) string {
	own := "https://tracker.own.example/announce?passkey=" + trkPasskey
	foreign := "https://cross.other.example/" + trkPasskey + "/announce"
	return trkState(t,
		// two private records with a foreign host, one of them listing it twice
		trkRecord{1, true, [][]string{{own}, {foreign}}},
		trkRecord{2, true, [][]string{{own, foreign, "https://cross.other.example/b/announce"}, {"udp://x.other2.example:80/announce"}}},
		// own domain incl. a subdomain: not foreign
		trkRecord{3, true, [][]string{{own, "https://sub.own.example/announce?passkey=" + trkPasskey}}},
		// co.uk: own domain is tracker.co.uk's registrable domain
		trkRecord{4, true, [][]string{{"https://a.site.co.uk/announce"}, {"https://b.site.co.uk/announce", "https://other.co.uk/announce"}}},
		// public record with the same foreign host
		trkRecord{5, false, [][]string{{"udp://open.example:1337/announce", foreign}}},
	)
}

func TestTrackersAuditCountsForeignHosts(t *testing.T) {
	dir := trkFixture(t)
	code, out, errOut := runTrk("audit", "--state", dir)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	noLeak(t, out, errOut)
	for _, want := range []string{
		"cross.other.example  records=3  private=2\n",
		"x.other2.example  records=1  private=1\n",
		"other.co.uk  records=1  private=1\n",
		"scanned=5 private=4 public=1 foreign_records=4 same_site_records=0 unparsed_trackers=0 no_primary=0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	hostRows := out[:strings.Index(out, "primaries")]
	if strings.Contains(hostRows, "sub.own.example") || strings.Contains(hostRows, "site.co.uk") {
		t.Errorf("own domain reported as foreign:\n%s", out)
	}
	// sorted by count: the 3-record host comes first
	if strings.Index(out, "cross.other.example") > strings.Index(out, "x.other2.example") {
		t.Errorf("not sorted by count:\n%s", out)
	}
}

func TestTrackersAuditHashesAndJSON(t *testing.T) {
	dir := trkFixture(t)
	code, out, errOut := runTrk("audit", "--state", dir, "--hashes")
	if code != 0 {
		t.Fatalf("exit %d, %q", code, errOut)
	}
	noLeak(t, out, errOut)
	if !strings.Contains(out, "  "+trkHash(1).HexString()+"\n") || !strings.Contains(out, "  "+trkHash(5).HexString()+"\n") {
		t.Errorf("hashes missing:\n%s", out)
	}
	_, plain, _ := runTrk("audit", "--state", dir)
	if strings.Contains(plain, trkHash(1).HexString()) {
		t.Error("hashes printed without --hashes")
	}

	code, js, errOut := runTrk("audit", "--state", dir, "--json", "--hashes")
	if code != 0 {
		t.Fatalf("exit %d, %q", code, errOut)
	}
	noLeak(t, js, errOut)
	var got struct {
		Hosts []struct {
			Host    string   `json:"host"`
			Records int      `json:"records"`
			Private int      `json:"private"`
			Hashes  []string `json:"hashes"`
		} `json:"hosts"`
		Scanned        int `json:"scanned"`
		Private        int `json:"private"`
		Public         int `json:"public"`
		ForeignRecords int `json:"foreign_records"`
	}
	dec := json.NewDecoder(strings.NewReader(js))
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("not json: %v\n%s", err, js)
	}
	if got.Scanned != 5 || got.Private != 4 || got.Public != 1 || got.ForeignRecords != 4 {
		t.Errorf("summary = %+v", got)
	}
	if len(got.Hosts) != 3 || got.Hosts[0].Host != "cross.other.example" || got.Hosts[0].Records != 3 ||
		got.Hosts[0].Private != 2 || len(got.Hosts[0].Hashes) != 3 {
		t.Errorf("hosts = %+v", got.Hosts)
	}
}

func TestTrackersAuditWritesNothing(t *testing.T) {
	dir := trkFixture(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	if code, _, e := runTrk("audit", "--state", dir); code != 0 {
		t.Fatalf("exit %d %q", code, e)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	if !bytes.Equal(before, after) {
		t.Error("audit changed config.db")
	}
}

func TestTrackersRemoveHostDryRunWritesNothing(t *testing.T) {
	dir := trkFixture(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	code, out, errOut := runTrk("remove-host", "--dry-run", "--hashes", "--state", dir, "cross.other.example")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	noLeak(t, out, errOut)
	if !strings.Contains(out, "records_changed=3 trackers_removed=4 private=2 public=1") || !strings.Contains(out, "dry run") {
		t.Errorf("summary:\n%s", out)
	}
	if !strings.Contains(out, trkHash(2).HexString()) {
		t.Errorf("--hashes lists nothing:\n%s", out)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	if !bytes.Equal(before, after) {
		t.Error("dry run changed config.db")
	}
}

func TestTrackersRemoveHostKeepsOrderAndDropsEmptyTiers(t *testing.T) {
	dir := trkFixture(t)
	own := "https://tracker.own.example/announce?passkey=" + trkPasskey
	code, out, errOut := runTrk("remove-host", "--state", dir, "Cross.Other.Example.")
	if code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	noLeak(t, out, errOut)
	if !strings.Contains(out, "records_changed=3 trackers_removed=4") {
		t.Errorf("summary:\n%s", out)
	}
	if got, want := trkTiers(t, dir, 1), [][]string{{own}}; !reflect.DeepEqual(got, want) {
		t.Errorf("rec1 tiers = %v, want %v (empty tier dropped)", got, want)
	}
	if got, want := trkTiers(t, dir, 2), [][]string{{own}, {"udp://x.other2.example:80/announce"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("rec2 tiers = %v, want %v", got, want)
	}
	if got := trkTiers(t, dir, 3); len(got) != 1 || len(got[0]) != 2 {
		t.Errorf("untouched record changed: %v", got)
	}
	if got, want := trkTiers(t, dir, 5), [][]string{{"udp://open.example:1337/announce"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("public record tiers = %v", got)
	}

	// Only the trackers changed: every other field, and the info hash, are intact.
	db, _ := settings.OpenOffline(dir, time.Second, true)
	var rec settings.TorrentDB
	err := json.Unmarshal(db.Get("Torrents", trkHash(2).HexString()), &rec)
	db.CloseDB()
	if err != nil {
		t.Fatal(err)
	}
	if rec.InfoHash != trkHash(2) || rec.Title != "t" || rec.Category != "movie" || rec.Size != 7 || rec.Timestamp != 2 ||
		!bytes.Equal(rec.InfoBytes, trkInfo(t, true)) {
		t.Errorf("record changed beyond its trackers: %+v", rec)
	}

	// A second run finds nothing, and the host stays gone from the audit.
	_, out2, _ := runTrk("audit", "--state", dir)
	if strings.Contains(out2, "cross.other.example") {
		t.Errorf("host came back:\n%s", out2)
	}
	_, out3, _ := runTrk("remove-host", "--state", dir, "cross.other.example")
	if !strings.Contains(out3, "records_changed=0 trackers_removed=0") {
		t.Errorf("second run:\n%s", out3)
	}
}

func TestTrackersRemoveHostRefusesAPrimaryHost(t *testing.T) {
	dir := trkFixture(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	// tracker.own.example is the primary of records 1, 2 and 3.
	code, out, errOut := runTrk("remove-host", "--state", dir, "tracker.own.example")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (out %q)", code, out)
	}
	if !strings.Contains(errOut, "in the primary tracker's own domain for 3 record") {
		t.Errorf("stderr lacks the count: %q", errOut)
	}
	noLeak(t, out, errOut)
	after, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	if !bytes.Equal(before, after) {
		t.Error("refused run changed config.db")
	}
}

func TestTrackersLockedDBExits2(t *testing.T) {
	dir := trkFixture(t)
	held, err := settings.OpenOffline(dir, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer held.CloseDB()
	old := trackersLockTimeout
	trackersLockTimeout = 100 * time.Millisecond
	defer func() { trackersLockTimeout = old }()
	for _, args := range [][]string{
		{"audit", "--state", dir},
		{"remove-host", "--state", dir, "cross.other.example"},
	} {
		code, out, errOut := runTrk(args...)
		if code != 2 || !strings.Contains(errOut, "config.db is in use: stop flodni first") {
			t.Errorf("%v: exit %d, stderr %q, stdout %q", args, code, errOut, out)
		}
	}
}

func TestTrackersUsageErrors(t *testing.T) {
	dir := trkFixture(t)
	for _, args := range [][]string{
		nil,
		{"nope"},
		{"audit", "--state", dir, "extra"},
		{"remove-host", "--state", dir},
		{"remove-host", "cross.other.example", "--dry-run"}, // flag after the host would be ignored
		{"remove-host", "--state", dir, ""},
		{"audit", "--state", t.TempDir()}, // no config.db there
	} {
		if code, _, _ := runTrk(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	// root without --allow-root
	old := scrubEuid
	scrubEuid = func() int { return 0 }
	defer func() { scrubEuid = old }()
	var o, e bytes.Buffer
	if code := trackersMain([]string{"audit", "--state", dir}, &o, &e); code != 2 || !strings.Contains(e.String(), "root") {
		t.Errorf("root: exit %d %q", code, e.String())
	}
	if code := trackersMain([]string{"audit", "--allow-root", "--state", dir}, &o, &e); code != 0 {
		t.Errorf("root with --allow-root: exit %d %q", code, e.String())
	}
}

func TestOwnDomain(t *testing.T) {
	for host, want := range map[string]string{
		"tracker.example.com": "example.com",
		"example.com":         "example.com",
		"a.b.site.co.uk":      "site.co.uk",
		"site.org.uk":         "site.org.uk",
		"x.site.com.hu":       "site.com.hu",
		"localhost":           "localhost",
		"10.0.0.5":            "10.0.0.5",
		"::1":                 "::1",
	} {
		if got := ownDomain(host); got != want {
			t.Errorf("ownDomain(%q) = %q, want %q", host, got, want)
		}
	}
}

// A host whose site name is the primary's (the primary on one top-level domain, this
// one on another) is reported apart, never hidden: it is usually the tracker's own
// second domain, but a lookalike reads the same, so the owner decides.
func siblingFixture(t *testing.T) string {
	primary := "https://tracker.own.example/announce?passkey=" + trkPasskey
	sibling := "https://t.own.org/announce?passkey=" + trkPasskey
	foreign := "https://cross.other.example/" + trkPasskey + "/announce"
	return trkState(t,
		trkRecord{1, true, [][]string{{primary}, {sibling}}},
		trkRecord{2, true, [][]string{{primary, sibling, foreign}}},
	)
}

func TestTrackersAuditReportsSameSiteHostsApart(t *testing.T) {
	code, out, errOut := runTrk("audit", "--state", siblingFixture(t))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	head := strings.Index(out, "same site, other domain")
	foreign := strings.Index(out, "cross.other.example  records=1  private=1\n")
	sibling := strings.Index(out, "t.own.org  records=2  private=2\n")
	if head < 0 || foreign < 0 || sibling < 0 {
		t.Fatalf("missing the foreign host, the same-site section or the sibling:\n%s", out)
	}
	if !(foreign < head && head < sibling) {
		t.Errorf("the sibling must be listed under the same-site section, the foreign host above it:\n%s", out)
	}
	if !strings.Contains(out, "foreign_records=1 same_site_records=2 unparsed_trackers=0 no_primary=0\n") {
		t.Errorf("summary:\n%s", out)
	}
	noLeak(t, out, errOut)
	_, js, _ := runTrk("audit", "--json", "--state", siblingFixture(t))
	if !strings.Contains(js, `"host":"t.own.org","records":2,"private":2,"same_site":true`) {
		t.Errorf("JSON lacks same_site for the sibling: %s", js)
	}
}

func TestTrackersRemoveHostNeedsAllowSameSiteForASibling(t *testing.T) {
	dir := siblingFixture(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	code, out, errOut := runTrk("remove-host", "--state", dir, "t.own.org")
	if code != 2 || !strings.Contains(errOut, "--allow-same-site") {
		t.Fatalf("exit %d, stderr %q, want a refusal naming --allow-same-site (out %q)", code, errOut, out)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "config.db")); !bytes.Equal(before, after) {
		t.Error("refused run changed config.db")
	}
	// the owner can still remove a lookalike on purpose
	if code, out, errOut := runTrk("remove-host", "--allow-same-site", "--state", dir, "t.own.org"); code != 0 || !strings.Contains(out, "records_changed=2") {
		t.Fatalf("with --allow-same-site: exit %d, out %q, stderr %q", code, out, errOut)
	}
	// a genuinely foreign host needs no flag
	if code, _, errOut := runTrk("remove-host", "--state", dir, "cross.other.example"); code != 0 {
		t.Fatalf("removing the foreign host: exit %d, stderr %q", code, errOut)
	}
}

func TestTrackersRemoveHostRefusesThePrimarysOwnDomain(t *testing.T) {
	dir := trkFixture(t)
	// sub.own.example is in the primary tracker.own.example's own domain (record 3)
	code, _, errOut := runTrk("remove-host", "--allow-same-site", "--state", dir, "sub.own.example")
	if code != 2 || !strings.Contains(errOut, "primary tracker's own domain") {
		t.Fatalf("exit %d, stderr %q: the primary's own domain must never be removable", code, errOut)
	}
}

func TestSiteName(t *testing.T) {
	for host, want := range map[string]string{
		"t.ncore.example":     "ncore",
		"ncore.other":         "ncore",
		"a.b.site.co.uk":      "site",
		"x.site.com.hu":       "site",
		"tracker.example.com": "example",
		"localhost":           "localhost",
		"10.0.0.5":            "10.0.0.5",
	} {
		if got := siteName(host); got != want {
			t.Errorf("siteName(%q) = %q, want %q", host, got, want)
		}
	}
}

// A flag after the host is a usage error that names the flag and says nothing
// changed, so it does not read like the safety refusal.
func TestTrackersTrailingFlagIsNamed(t *testing.T) {
	dir := trkFixture(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	code, _, errOut := runTrk("remove-host", "cross.other.example", "--state", dir)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	first := strings.SplitN(errOut, "\n", 2)[0]
	if !strings.Contains(first, "--state must come before <host>") || !strings.Contains(first, "nothing was changed") {
		t.Errorf("first line does not name the misplaced flag: %q", first)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "config.db")); !bytes.Equal(before, after) {
		t.Error("config.db changed")
	}
	if _, _, errOut := runTrk("remove-host", "cross.other.example", "--dry-run=true"); !strings.Contains(errOut, "--dry-run must come before") {
		t.Errorf("a --flag=value form is not named: %q", errOut)
	}
}

// A host that is same-site for one record and foreign for another gets one row per
// class, each with its own count; the same-site header appears only with rows.
func TestTrackersAuditMixedHostGetsTwoRows(t *testing.T) {
	dir := trkState(t,
		trkRecord{1, true, [][]string{{"https://tracker.own.example/a"}, {"https://t.own.org/a"}}},
		trkRecord{2, true, [][]string{{"https://tracker.own.example/a"}, {"https://t.own.org/a"}}},
		trkRecord{3, true, [][]string{{"https://tracker.zzz.example/a"}, {"https://t.own.org/a"}}},
	)
	code, out, errOut := runTrk("audit", "--state", dir)
	if code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	head := strings.Index(out, "same site, other domain")
	foreign := strings.Index(out, "t.own.org  records=1  private=1\n")
	same := strings.Index(out, "t.own.org  records=2  private=2\n")
	if head < 0 || foreign < 0 || same < 0 || !(foreign < head && head < same) {
		t.Errorf("want a foreign row (1) above the header and a same-site row (2) below:\n%s", out)
	}
	_, js, _ := runTrk("audit", "--json", "--state", dir)
	if !strings.Contains(js, `"host":"t.own.org","records":2,"private":2,"same_site":true`) ||
		!strings.Contains(js, `"host":"t.own.org","records":1,"private":1,"same_site":false`) {
		t.Errorf("JSON rows: %s", js)
	}
	// no same-site rows, no header
	_, out2, _ := runTrk("audit", "--state", trkFixture(t))
	if strings.Contains(out2, "same site, other domain") {
		t.Errorf("header without rows:\n%s", out2)
	}
}

func TestTrackersAuditReportsPrimariesAndUnparsed(t *testing.T) {
	dir := trkState(t,
		trkRecord{1, true, [][]string{{"https://tracker.own.example/a"}}},
		trkRecord{2, true, [][]string{{"https://tracker.own.example/a"}, {"https://cross.other.example/a"}}},
		trkRecord{3, true, [][]string{{"https://cross.other.example/a"}, {"https://tracker.own.example/a"}}},
		trkRecord{4, true, [][]string{{"no-scheme.example/a", "::bad"}}}, // no parsable host
		trkRecord{5, false, nil},
	)
	code, out, errOut := runTrk("audit", "--state", dir)
	if code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	noLeak(t, out, errOut)
	for _, want := range []string{"primaries", "own.example  records=2\n", "other.example  records=1\n", "unparsed_trackers=2 no_primary=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	_, js, _ := runTrk("audit", "--json", "--state", dir)
	for _, want := range []string{`"primaries":[{"site":"own.example","records":2},{"site":"other.example","records":1}]`, `"unparsed_trackers":2`, `"no_primary":2`} {
		if !strings.Contains(js, want) {
			t.Errorf("JSON lacks %q: %s", want, js)
		}
	}
}

// A record whose .torrent lists the foreign tracker first has it as its primary: the
// run refuses, and --hashes says which records.
func TestTrackersRemoveHostRefusalListsHashes(t *testing.T) {
	foreignFirst := trkState(t,
		trkRecord{1, true, [][]string{{"https://cross.other.example/a"}, {"https://tracker.own.example/a"}}},
		trkRecord{2, true, [][]string{{"https://tracker.own.example/a"}, {"https://cross.other.example/a"}}},
	)
	before, _ := os.ReadFile(filepath.Join(foreignFirst, "config.db"))
	code, out, errOut := runTrk("remove-host", "--hashes", "--state", foreignFirst, "cross.other.example")
	if code != 2 || !strings.Contains(errOut, "primary tracker's own domain for 1 record") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, trkHash(1).HexString()) || strings.Contains(out, trkHash(2).HexString()) {
		t.Errorf("want only the refusing record's hash:\n%s", out)
	}
	if _, plain, _ := runTrk("remove-host", "--state", foreignFirst, "cross.other.example"); strings.Contains(plain, trkHash(1).HexString()) {
		t.Error("hash printed without --hashes")
	}
	if after, _ := os.ReadFile(filepath.Join(foreignFirst, "config.db")); !bytes.Equal(before, after) {
		t.Error("refused run changed config.db")
	}

	code, out, errOut = runTrk("remove-host", "--hashes", "--state", siblingFixture(t), "t.own.org")
	if code != 2 || !strings.Contains(errOut, "--allow-same-site") ||
		!strings.Contains(out, trkHash(1).HexString()) || !strings.Contains(out, trkHash(2).HexString()) {
		t.Errorf("same-site refusal: exit %d, out %q, stderr %q", code, out, errOut)
	}
}

func TestTrackersRemoveHostRejectsPortAndBrackets(t *testing.T) {
	dir := trkFixture(t)
	before, _ := os.ReadFile(filepath.Join(dir, "config.db"))
	for arg, want := range map[string]string{"cross.other.example:443": "port", "[::1]": "brackets", "[::1]:80": "brackets"} {
		code, _, errOut := runTrk("remove-host", "--state", dir, arg)
		if code != 2 || !strings.Contains(strings.SplitN(errOut, "\n", 2)[0], want) {
			t.Errorf("%q: exit %d, stderr %q, want a usage error naming the %s", arg, code, errOut, want)
		}
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "config.db")); !bytes.Equal(before, after) {
		t.Error("config.db changed")
	}
	// a bare IPv6 address is a host, not a port
	if code, out, errOut := runTrk("remove-host", "--dry-run", "--state", dir, "::1"); code != 0 || !strings.Contains(out, "no record has that host") {
		t.Errorf("bare ipv6: exit %d, out %q, stderr %q", code, out, errOut)
	}
}

func TestTrackersRemoveHostSaysWhenNoRecordHasTheHost(t *testing.T) {
	code, out, _ := runTrk("remove-host", "--state", trkFixture(t), "nobody.example")
	if code != 0 || !strings.Contains(out, "no record has that host") || !strings.Contains(out, "records_changed=0") {
		t.Errorf("exit %d, out %q", code, out)
	}
}

// Fields the struct does not know, with bytes a decode-and-compare would not tell
// apart (number formats, key order, escapes), must survive byte for byte.
func TestTrackersRemoveHostKeepsOtherFieldsRaw(t *testing.T) {
	dir := trkFixture(t)
	key := trkHash(2).HexString()
	open := func(ro bool) *settings.TDB {
		db, err := settings.OpenOffline(dir, time.Second, ro)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open(false)
	var m map[string]json.RawMessage
	if err := json.Unmarshal(db.Get("Torrents", key), &m); err != nil {
		t.Fatal(err)
	}
	m["Extra"] = json.RawMessage(`{"z":1.0,"a":[1e2,"\u003c"],"big":12345678901234567890}`)
	m["Title"] = json.RawMessage(`"t\u00e9"`)
	seeded, _ := json.Marshal(m)
	if err := db.Put("Torrents", key, seeded); err != nil {
		t.Fatal(err)
	}
	orig := db.Get("Torrents", key)
	db.CloseDB()

	if code, _, errOut := runTrk("remove-host", "--state", dir, "cross.other.example"); code != 0 {
		t.Fatalf("exit %d %q", code, errOut)
	}
	db = open(true)
	defer db.CloseDB()
	var o, n map[string]json.RawMessage
	if err := json.Unmarshal(orig, &o); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(db.Get("Torrents", key), &n); err != nil {
		t.Fatal(err)
	}
	if len(o) != len(n) {
		t.Errorf("key set changed: %d -> %d", len(o), len(n))
	}
	for k, v := range o {
		if k != "Trackers" && !bytes.Equal(v, n[k]) {
			t.Errorf("field %s changed: %s -> %s", k, v, n[k])
		}
	}
	if bytes.Equal(o["Trackers"], n["Trackers"]) {
		t.Error("Trackers did not change")
	}
}

// If the bytes about to be written do not match the plan, nothing is written, only the
// hash is printed, and the run stops with exit 1.
func TestTrackersRemoveHostVerifiesBeforeWriting(t *testing.T) {
	for name, bad := range map[string]func(raw []byte, tiers [][]string) ([]byte, error){
		"other field changed": func(raw []byte, tiers [][]string) ([]byte, error) {
			var m map[string]json.RawMessage
			_ = json.Unmarshal(raw, &m)
			m["Title"] = json.RawMessage(`"changed"`)
			tr, _ := json.Marshal(tiers)
			m["Trackers"] = tr
			return json.Marshal(m)
		},
		"trackers not the plan": func(raw []byte, tiers [][]string) ([]byte, error) {
			return rewriteTrackers(raw, [][]string{{"https://tracker.own.example/other"}})
		},
		"not json": func([]byte, [][]string) ([]byte, error) { return []byte("{"), nil },
	} {
		t.Run(name, func(t *testing.T) {
			dir := trkFixture(t)
			before, _ := os.ReadFile(filepath.Join(dir, "config.db"))
			old := trackersRewrite
			trackersRewrite = bad
			defer func() { trackersRewrite = old }()
			code, out, errOut := runTrk("remove-host", "--state", dir, "cross.other.example")
			if code != 1 || !strings.Contains(errOut, "could not rewrite record "+trkHash(1).HexString()) {
				t.Fatalf("exit %d, out %q, stderr %q", code, out, errOut)
			}
			if strings.Contains(out, "records_changed") {
				t.Errorf("summary printed after a failed run: %q", out)
			}
			noLeak(t, out, errOut)
			if after, _ := os.ReadFile(filepath.Join(dir, "config.db")); !bytes.Equal(before, after) {
				t.Error("config.db changed")
			}
		})
	}
}
