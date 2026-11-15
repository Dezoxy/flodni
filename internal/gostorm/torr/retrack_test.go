package torr

import (
	"bytes"
	"mime/multipart"
	"reflect"
	"testing"
	"time"

	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torrshash"
	apiutils "flodni/internal/gostorm/web/api/utils"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func testInfoBytes(t *testing.T, private *bool) []byte {
	t.Helper()
	b, err := bencode.Marshal(metainfo.Info{
		Name:        "release",
		Length:      16384,
		PieceLength: 16384,
		Pieces:      make([]byte, 20),
		Private:     private,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A private torrent (BEP 27) gets no tracker beyond its own metainfo; a public
// torrent and one whose metadata is not known yet keep the pre-fix behaviour
// where the info is known, and wait for the metadata where it is not.
func TestRetrackSpecKeepsPrivateTorrentsPrivate(t *testing.T) {
	yes, no := true, false
	own := [][]string{{"http://seeder.example/announce"}}
	def := func() []string { return []string{"udp://public.example:1337/announce"} }
	file := func() []string { return []string{"udp://file.example:80/announce"} }
	withDefs := [][]string{
		{"http://seeder.example/announce"},
		{"udp://public.example:1337/announce"},
		{"udp://file.example:80/announce"},
	}
	cases := []struct {
		name string
		info []byte
		mode int
		want [][]string
	}{
		{"private, mode 1", testInfoBytes(t, &yes), 1, own},
		{"private, mode 2", testInfoBytes(t, &yes), 2, own},
		{"private, mode 3", testInfoBytes(t, &yes), 3, own},
		{"private, mode 0", testInfoBytes(t, &yes), 0, own},
		{"magnet without info, mode 1", nil, 1, own},
		{"magnet without info, mode 3", nil, 3, own},
		{"public, mode 1", testInfoBytes(t, &no), 1, withDefs},
		{"no flag, mode 1", testInfoBytes(t, nil), 1, withDefs},
		{"public, mode 2", testInfoBytes(t, &no), 2, [][]string{{"udp://file.example:80/announce"}}},
		{"public, mode 3", testInfoBytes(t, &no), 3, [][]string{
			{"udp://public.example:1337/announce"},
			{"udp://file.example:80/announce"},
		}},
	}
	for _, c := range cases {
		spec := &torrent.TorrentSpec{InfoBytes: c.info, Trackers: append([][]string(nil), own...)}
		retrackSpec(spec, c.mode, def, file)
		if !reflect.DeepEqual(spec.Trackers, c.want) {
			t.Errorf("%s: trackers = %v, want %v", c.name, spec.Trackers, c.want)
		}
	}
}

func TestInfoPublic(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		info *metainfo.Info
		want bool
	}{
		{"not known yet", nil, false},
		{"private", &metainfo.Info{Private: &yes}, false},
		{"public", &metainfo.Info{Private: &no}, true},
		{"no flag", &metainfo.Info{}, true},
	}
	for _, c := range cases {
		if got := infoPublic(c.info); got != c.want {
			t.Errorf("%s: infoPublic = %v, want %v", c.name, got, c.want)
		}
	}
}

// A private torrent woken from its stored spec loses the public trackers it was saved
// with (the library defaults, a downloaded retracker) and keeps its own; a public
// torrent keeps everything.
func TestDropPublicTrackersOnWake(t *testing.T) {
	yes, no := true, false
	public := func() []string {
		return []string{
			"udp://tracker.opentrackr.org:1337/announce",
			"udp://open.stealth.si:80/announce",
			"udp://tracker.torrent.eu.org:451/announce",
			"udp://exodus.desync.com:6969/announce",
			"udp://tracker.openbittorrent.com:6969/announce",
			"udp://retracker.example:80/announce",
		}
	}
	own := "http://seeder.example/announce"
	extra := "https://other.example/announce"
	stored := func() [][]string {
		return [][]string{{own}, public()[:5], {public()[5]}, {extra}}
	}
	cases := []struct {
		name string
		info []byte
		want [][]string
	}{
		{"private", testInfoBytes(t, &yes), [][]string{{own}, {extra}}},
		{"public", testInfoBytes(t, &no), stored()},
		{"info unknown", nil, stored()},
	}
	for _, c := range cases {
		spec := &torrent.TorrentSpec{InfoBytes: c.info, Trackers: stored()}
		dropPublicTrackers(spec, public)
		if !reflect.DeepEqual(spec.Trackers, c.want) {
			t.Errorf("%s: trackers = %v, want %v", c.name, spec.Trackers, c.want)
		}
	}
}

// A wake (LoadStored, reached from native.Wake and WakeStored) adds a closed torrent
// from its stored record alone: the spec NewTorrent gets is the record's own, with no
// magnet parsed and no tracker merged in. For a private torrent that spec must then
// carry its own tracker only, whatever the retracker mode (hard rule 3). The spec
// here is the one the real wake path hands to the engine.
func TestWakeOfAStoredPrivateTorrentKeepsOnlyItsOwnTrackers(t *testing.T) {
	warmupFixture(t)
	yes := true
	own := "http://seeder.example/announce"
	def := func() []string { return []string{"udp://public.example:1337/announce"} }
	file := func() []string { return []string{"udp://file.example:80/announce"} }
	public := func() []string { return append(def(), file()...) }
	h := metainfo.NewHashFromHex(removeTestHash)
	// A record saved before the retracker gate carries the public lists too.
	sets.AddTorrent(&sets.TorrentDB{TorrentSpec: &torrent.TorrentSpec{
		InfoHash:  h,
		InfoBytes: testInfoBytes(t, &yes),
		Trackers:  [][]string{{own}, def(), file()},
	}})

	var got *torrent.TorrentSpec
	prev := newTorrent
	newTorrent = func(spec *torrent.TorrentSpec, _ *BTServer) (*Torrent, error) {
		got = spec
		return &Torrent{TorrentSpec: spec}, nil
	}
	defer func() { newTorrent = prev }()

	for mode := 0; mode <= 3; mode++ {
		got = nil
		if _, err := LoadStored(h.HexString()); err != nil {
			t.Fatalf("mode %d: LoadStored: %v", mode, err)
		}
		if got == nil || len(got.InfoBytes) == 0 {
			t.Fatalf("mode %d: the wake did not hand the record's metainfo to the engine", mode)
		}
		// What NewTorrent applies before the client sees the spec.
		dropPublicTrackers(got, public)
		retrackSpec(got, mode, def, file)
		if want := [][]string{{own}}; !reflect.DeepEqual(got.Trackers, want) {
			t.Errorf("mode %d: trackers = %v, want %v", mode, got.Trackers, want)
		}
	}
}

// Hard rule 3 for the tracker scrape: a private torrent's scrape targets only
// its own announce hosts, whatever the retracker mode added at the add and
// whatever an older record carries. A torrent whose metadata is not known yet
// is treated the same way; a public torrent keeps its stored trackers.
func TestScrapeTargetsKeepPrivateTorrentsOnTheirOwnTrackers(t *testing.T) {
	yes, no := true, false
	own := "http://seeder.example/test-key-not-real/announce?auth=test-auth-not-real"
	public := []string{"udp://public.example:1337/announce", "udp://file.example:80/announce"}
	def := func() []string { return public[:1] }
	file := func() []string { return public[1:] }

	record := func(info []byte, mode int, stored [][]string) *sets.TorrentDB {
		spec := &torrent.TorrentSpec{InfoBytes: info, Trackers: append([][]string(nil), stored...)}
		spec.InfoHash[0] = 0xab
		retrackSpec(spec, mode, def, file)
		return &sets.TorrentDB{TorrentSpec: spec}
	}
	ownTier := [][]string{{own}}
	// an older record that was saved with the public lists in it
	legacy := [][]string{{own}, {public[0], public[1]}}

	cases := []struct {
		name string
		db   *sets.TorrentDB
		want []string
	}{
		{"private, retracker mode 1", record(testInfoBytes(t, &yes), 1, ownTier), []string{own}},
		{"private, retracker mode 3", record(testInfoBytes(t, &yes), 3, ownTier), []string{own}},
		{"private, older record holding public lists", record(testInfoBytes(t, &yes), 0, legacy), []string{own}},
		{"metadata not known, older record", record(nil, 0, legacy), []string{own}},
		{"public, mode 1", record(testInfoBytes(t, &no), 1, ownTier), []string{own, public[0], public[1]}},
		// a tier of an older, rotated list: one entry no longer in any list, one still
		// there; the whole tier goes
		{"private, tier with a rotated-out entry", record(testInfoBytes(t, &yes), 0,
			[][]string{{own}, {"udp://rotated-out.example:6969/announce", public[0]}}), []string{own}},
		// the same host spelled another way: scheme, case, path, port
		{"private, host variants", record(testInfoBytes(t, &yes), 0, [][]string{
			{own},
			{"HTTP://Public.EXAMPLE:8080/other/path"},
			{"udp://FILE.example:81/announce"},
		}), []string{own}},
		{"metadata not known, host variant", record(nil, 0, [][]string{{own}, {"https://Public.Example/announce"}}), []string{own}},
	}
	for _, c := range cases {
		got := scrapeTargetsOf([]*sets.TorrentDB{c.db}, func() []string { return public })
		if len(got) != 1 || !reflect.DeepEqual(got[0].Trackers, c.want) {
			t.Errorf("%s: targets = %+v, want trackers %v", c.name, got, c.want)
		}
	}
	if got := scrapeTargetsOf([]*sets.TorrentDB{nil, {}}, func() []string { return nil }); len(got) != 0 {
		t.Errorf("empty records give targets: %+v", got)
	}
}

// Tracker injection (hard rule 3): an add for a stored hash that is not known public
// must not carry the request's trackers into the engine, the saved record or the next
// wake. Every add path (POST /torrents with a magnet or a torrs:// link, /stream,
// /torrent/upload, and the library's add and inspect, which reach the engine through
// POST /torrents) goes through AddTorrent, so these tests drive it with the specs
// those parsers build.

const (
	injectHash = "0123456789abcdef0123456789abcdef01234567"
	foreignTr  = "http://evil.invalid/announce"
	ownTr      = "http://seeder.example/announce?passkey=x"
)

func injectFixture(t *testing.T, private *bool) (h metainfo.Hash, info []byte) {
	t.Helper()
	oldPath, oldRO := sets.Path, sets.ReadOnly
	sets.Path = t.TempDir()
	sets.InitSets(false, false)
	InitApiHelper(NewBTS())
	t.Cleanup(func() {
		InitApiHelper(nil)
		sets.Path, sets.ReadOnly = oldPath, oldRO
	})
	// The stored record's hash is the hash of its own metainfo, so a .torrent file
	// for it can be built.
	info = testInfoBytes(t, private)
	h = metainfo.HashBytes(info)
	sets.AddTorrent(&sets.TorrentDB{TorrentSpec: &torrent.TorrentSpec{
		InfoHash: h, InfoBytes: info, Trackers: [][]string{{ownTr}},
	}})
	return h, info
}

type namedFile struct{ *bytes.Reader }

func (namedFile) Close() error { return nil }

var _ multipart.File = namedFile{}

func TestAddForStoredPrivateHashIgnoresRequestTrackers(t *testing.T) {
	yes := true
	h, info := injectFixture(t, &yes)
	hex := h.HexString()

	torrsTok, err := torrshash.Pack(func() *torrshash.TorrsHash {
		th := torrshash.New(hex)
		th.AddField(torrshash.TagTracker, foreignTr)
		return th
	}())
	if err != nil {
		t.Fatal(err)
	}
	mi, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: info, Announce: foreignTr})
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(t *testing.T) *torrent.TorrentSpec{
		"magnet": func(t *testing.T) *torrent.TorrentSpec {
			s, err := apiutils.ParseLink("magnet:?xt=urn:btih:" + hex + "&tr=" + foreignTr)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"torrs": func(t *testing.T) *torrent.TorrentSpec {
			s, _, err := apiutils.ParseTorrsHash("torrs://" + torrsTok)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"upload": func(t *testing.T) *torrent.TorrentSpec {
			s, err := apiutils.ParseFile(namedFile{bytes.NewReader(mi)})
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			spec := mk(t)
			if !reflect.DeepEqual(spec.Trackers, [][]string{{foreignTr}}) {
				t.Fatalf("fixture: request trackers = %v", spec.Trackers)
			}
			bt := NewBTS()
			InitApiHelper(bt)
			var got *torrent.TorrentSpec
			prev := newTorrent
			newTorrent = func(s *torrent.TorrentSpec, bt *BTServer) (*Torrent, error) {
				got = s
				return register(bt, s), nil
			}
			defer func() { newTorrent = prev }()

			tor, err := AddTorrent(spec, "", "", "", "")
			if err != nil {
				t.Fatal(err)
			}
			want := [][]string{{ownTr}}
			if !reflect.DeepEqual(got.Trackers, want) {
				t.Errorf("engine spec trackers = %v, want %v", got.Trackers, want)
			}

			// The idle close saves the wrapper's spec: the record keeps only its own.
			tor.expiredTime = time.Now().Add(-time.Second)
			setOnIdleClose(t, nil)
			tor.UpdateStats()
			rec := GetTorrentDB(h)
			if rec == nil || !reflect.DeepEqual(rec.Trackers, want) {
				t.Errorf("saved record trackers = %v, want %v", rec.Trackers, want)
			}

			// The next wake replays the record.
			got = nil
			if _, err := LoadStored(hex); err != nil {
				t.Fatal(err)
			}
			if got == nil || !reflect.DeepEqual(got.Trackers, want) {
				t.Errorf("wake trackers = %v, want %v", got, want)
			}
		})
	}
}

// A record that is not known public (no metainfo) is treated as private.
func TestAddForRecordWithoutMetainfoIgnoresRequestTrackers(t *testing.T) {
	h, _ := injectFixture(t, nil)
	sets.AddTorrent(&sets.TorrentDB{TorrentSpec: &torrent.TorrentSpec{
		InfoHash: h, Trackers: [][]string{{ownTr}},
	}})
	var got *torrent.TorrentSpec
	prev := newTorrent
	newTorrent = func(s *torrent.TorrentSpec, bt *BTServer) (*Torrent, error) {
		got = s
		return register(bt, s), nil
	}
	defer func() { newTorrent = prev }()
	InitApiHelper(NewBTS())
	if _, err := AddTorrent(&torrent.TorrentSpec{InfoHash: h, Trackers: [][]string{{foreignTr}}}, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{ownTr}}; !reflect.DeepEqual(got.Trackers, want) {
		t.Errorf("trackers = %v, want %v", got.Trackers, want)
	}
}

// A known-public record still merges the request's trackers, as before.
func TestAddForStoredPublicHashStillMergesTrackers(t *testing.T) {
	no := false
	h, _ := injectFixture(t, &no)
	var got *torrent.TorrentSpec
	prev := newTorrent
	newTorrent = func(s *torrent.TorrentSpec, bt *BTServer) (*Torrent, error) {
		got = s
		return register(bt, s), nil
	}
	defer func() { newTorrent = prev }()
	InitApiHelper(NewBTS())
	if _, err := AddTorrent(&torrent.TorrentSpec{InfoHash: h, Trackers: [][]string{{"http://new.example/announce"}}}, "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"http://new.example/announce"}, {ownTr}}
	if !reflect.DeepEqual(got.Trackers, want) {
		t.Errorf("trackers = %v, want %v", got.Trackers, want)
	}
}

// A torrent the engine already holds (no record yet) takes no tracker from an add
// unless its metainfo shows it public: AddTorrentSpec would merge them into the live
// torrent.
func TestAddToHeldTorrentAddsNoTrackers(t *testing.T) {
	oldPath, oldRO := sets.Path, sets.ReadOnly
	sets.Path = t.TempDir()
	sets.InitSets(false, false)
	t.Cleanup(func() { sets.Path, sets.ReadOnly = oldPath, oldRO })
	cfg := torrent.NewDefaultClientConfig()
	cfg.NoDHT = true
	cfg.NoUpload = true
	cfg.ListenPort = 0
	cfg.DataDir = t.TempDir()
	cfg.DisableTrackers = true
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, tc := range []struct {
		name    string
		private *bool
		wantNew bool
	}{{"private", ptr(true), false}, {"public", ptr(false), true}} {
		t.Run(tc.name, func(t *testing.T) {
			info := testInfoBytes(t, tc.private)
			h := metainfo.HashBytes(info)
			bt := NewBTS()
			bt.client = client
			first, err := NewTorrent(&torrent.TorrentSpec{InfoHash: h, InfoBytes: info, Trackers: [][]string{{ownTr}}}, bt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewTorrent(&torrent.TorrentSpec{InfoHash: h, Trackers: [][]string{{foreignTr}}}, bt); err != nil {
				t.Fatal(err)
			}
			var live []string
			mi := first.Torrent.Metainfo()
			for _, tier := range mi.UpvertedAnnounceList() {
				live = append(live, tier...)
			}
			has := false
			for _, tr := range live {
				if tr == foreignTr {
					has = true
				}
			}
			if has != tc.wantNew {
				t.Errorf("foreign tracker on the live torrent = %v, want %v (trackers %v)", has, tc.wantNew, live)
			}
		})
	}
}

func ptr(b bool) *bool { return &b }

// A torrent held from a bare magnet has no metainfo yet, so it may be private: an add
// with foreign trackers must not reach the live torrent.
func TestAddToHeldTorrentWithoutMetainfoAddsNoTrackers(t *testing.T) {
	oldPath, oldRO := sets.Path, sets.ReadOnly
	sets.Path = t.TempDir()
	sets.InitSets(false, false)
	t.Cleanup(func() { sets.Path, sets.ReadOnly = oldPath, oldRO })
	cfg := torrent.NewDefaultClientConfig()
	cfg.NoDHT = true
	cfg.NoUpload = true
	cfg.ListenPort = 0
	cfg.DataDir = t.TempDir()
	cfg.DisableTrackers = true
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	h := metainfo.HashBytes(testInfoBytes(t, ptr(true)))
	bt := NewBTS()
	bt.client = client
	first, err := NewTorrent(&torrent.TorrentSpec{InfoHash: h, Trackers: [][]string{{ownTr}}}, bt)
	if err != nil {
		t.Fatal(err)
	}
	if first.Torrent.Info() != nil {
		t.Fatal("fixture: the held torrent must have no metainfo")
	}
	if _, err := NewTorrent(&torrent.TorrentSpec{InfoHash: h, Trackers: [][]string{{foreignTr}}}, bt); err != nil {
		t.Fatal(err)
	}
	mi := first.Torrent.Metainfo()
	for _, tier := range mi.UpvertedAnnounceList() {
		for _, tr := range tier {
			if tr == foreignTr {
				t.Fatalf("a held torrent without metainfo took a request tracker: %v", mi.UpvertedAnnounceList())
			}
		}
	}
}
