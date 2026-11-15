package library

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"flodni/internal/metadb"
)

// The replace policy (ADR 3, step 2).

type policyEnv struct {
	m       *Manager
	eng     *recEngine
	reg     *memRegistry
	tv      string
	oldStub map[int]string // episode -> stub of the release already filed
	added   *[]addedCall
}

// newPolicyEnv files the old release's episodes with the given stored scores (an
// absent episode has no row). The next add brings the new release, which names
// S01E01 and S01E02.
func newPolicyEnv(t *testing.T, prior map[int]int) *policyEnv {
	t.Helper()
	eng := &recEngine{}
	eng.files = []FileStat{
		{ID: 1, Path: "Show.S01/Show.S01E01.mkv", Length: 1 << 30},
		{ID: 2, Path: "Show.S01/Show.S01E02.mkv", Length: 1 << 30},
	}
	reg := &memRegistry{rows: map[string]metadb.EpisodeEntry{}}
	tv := t.TempDir()
	m := New(Config{TVDir: tv, GoStorm: eng, Registry: reg, GoStormURL: "http://engine"})
	var calls []addedCall
	m.cfg.OnAdded = func(hash string, files []AddedFile) { calls = append(calls, addedCall{hash, files}) }
	e := &policyEnv{m: m, eng: eng, reg: reg, tv: tv, oldStub: map[int]string{}, added: &calls}
	for ep, score := range prior {
		p := filepath.Join(tv, "Show", "Season.01", EpisodeFilename("Show", 1, ep, oldHash[:8]))
		if err := WriteStub(p, m.streamURL(oldHash, ep), 1000, oldHash, ""); err != nil {
			t.Fatal(err)
		}
		key := EpisodeKey("Show", 1, ep)
		reg.rows[key] = metadb.EpisodeEntry{EpisodeKey: key, Hash: oldHash, FilePath: p, Source: "api", QualityScore: score}
		e.oldStub[ep] = p
	}
	return e
}

func (e *policyEnv) add(episode int, policy string, score int) (*AddResponse, error) {
	return e.m.Add(context.Background(), AddRequest{
		Type: "tv", Title: "Show", Hash: newHash, Season: 1, Episode: episode,
		Replace: policy, QualityScore: score, MetadataWait: 1,
	})
}

func (e *policyEnv) newStub(ep int) string {
	return filepath.Join(e.tv, "Show", "Season.01", EpisodeFilename("Show", 1, ep, newHash[:8]))
}

// policy x prior: stubs, registry, engine calls and the answer, for a single episode.
func TestReplacePolicyTable(t *testing.T) {
	const stored = 5
	type want struct {
		decision string
		replaced bool
		engine   []string // calls, nil for none
	}
	engineAdd := []string{"AddTorrent", "GetTorrentInfo"}
	engineReplace := []string{"AddTorrent", "GetTorrentInfo", "ReadFileHead", "RemoveTorrent"}
	cases := []struct {
		name   string
		policy string
		prior  map[int]int
		score  int
		want   want
	}{
		{"never, no prior", "never", nil, 7, want{DecisionAdded, false, engineAdd}},
		{"never, prior", "never", map[int]int{1: stored}, 99, want{DecisionKept, false, nil}},
		{"always, no prior", "always", nil, 0, want{DecisionAdded, false, engineAdd}},
		{"always, prior with a higher score", "always", map[int]int{1: 90}, 1, want{DecisionReplaced, true, engineReplace}},
		// Omitted is always, and answers the v0.1.0 body: no decision field.
		{"omitted, prior with a higher score", "", map[int]int{1: 90}, 1, want{"", true, engineReplace}},
		{"if-better, no prior", "if-better", nil, 0, want{DecisionAdded, false, engineAdd}},
		{"if-better, prior with a lower score", "if-better", map[int]int{1: stored}, 7, want{DecisionReplaced, true, engineReplace}},
		{"if-better, equal score", "if-better", map[int]int{1: stored}, stored, want{DecisionKept, false, nil}},
		{"if-better, higher stored score", "if-better", map[int]int{1: 9}, 7, want{DecisionKept, false, nil}},
		{"if-better, score 0 against a stored 0", "if-better", map[int]int{1: 0}, 0, want{DecisionKept, false, nil}},
		{"if-better, score 0 against a stored 5", "if-better", map[int]int{1: stored}, 0, want{DecisionKept, false, nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newPolicyEnv(t, c.prior)
			before := map[string]metadb.EpisodeEntry{}
			for k, v := range e.reg.rows {
				before[k] = v
			}
			resp, err := e.add(1, c.policy, c.score)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Decision != c.want.decision {
				t.Fatalf("decision = %q, want %q (%+v)", resp.Decision, c.want.decision, resp)
			}
			if got := e.eng.seen(); !reflect.DeepEqual(got, c.want.engine) {
				t.Fatalf("engine calls = %v, want %v", got, c.want.engine)
			}
			key := EpisodeKey("Show", 1, 1)
			switch c.want.decision {
			case DecisionKept:
				if len(resp.Files) != 0 || resp.Files == nil {
					t.Fatalf("files = %#v, want an empty list", resp.Files)
				}
				if len(resp.Skipped) != 1 || resp.Skipped[0].Episode != 1 || resp.Skipped[0].Reason == "" {
					t.Fatalf("skipped = %+v", resp.Skipped)
				}
				if !exists(e.oldStub[1]) || exists(e.newStub(1)) {
					t.Fatal("a refused add changed the stubs")
				}
				if !reflect.DeepEqual(e.reg.rows, before) {
					t.Fatalf("a refused add changed the registry: %+v", e.reg.rows)
				}
				if len(*e.added) != 0 {
					t.Fatal("OnAdded called for a refused add")
				}
			default:
				if len(resp.Files) != 1 || resp.Files[0].Path != e.newStub(1) || !exists(e.newStub(1)) {
					t.Fatalf("files = %+v", resp.Files)
				}
				if got := e.reg.rows[key]; got.Hash != newHash || got.QualityScore != c.score {
					t.Fatalf("row = %+v", got)
				}
				if len(*e.added) != 1 {
					t.Fatalf("OnAdded calls = %d", len(*e.added))
				}
				if c.want.replaced {
					want := []ReplacedFile{{Path: e.oldStub[1], Hash: oldHash, FileIndex: 1}}
					if c.policy == "" {
						want = nil // the v0.1.0 body does not list it
					}
					if !reflect.DeepEqual(resp.Replaced, want) || exists(e.oldStub[1]) {
						t.Fatalf("replaced = %+v, old stub exists = %v", resp.Replaced, exists(e.oldStub[1]))
					}
				} else if len(resp.Replaced) != 0 {
					t.Fatalf("replaced = %+v, want none", resp.Replaced)
				}
			}
		})
	}
}

// The readability gate is a safety check, not a policy: a release that delivers
// nothing is refused with 422 under always and if-better, and the old files stay.
func TestReadabilityGateStillRefusesUnderEveryReplacingPolicy(t *testing.T) {
	for _, policy := range []string{"", "always", "if-better"} {
		t.Run("policy "+policy, func(t *testing.T) {
			e := newPolicyEnv(t, map[int]int{1: 3})
			e.eng.readErr = errors.New("no peers")
			_, err := e.add(1, policy, 8)
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
				t.Fatalf("err = %v, want 422", err)
			}
			if !exists(e.oldStub[1]) || exists(e.newStub(1)) || e.reg.rows[EpisodeKey("Show", 1, 1)].Hash != oldHash {
				t.Fatal("a refused replacement changed the library")
			}
		})
	}
}

// A partly allowed pack writes the allowed episodes and reports the refused ones in
// skipped, never in files.
func TestPartlyAllowedPackWritesTheAllowedEpisodes(t *testing.T) {
	// E01 stored at 5 (the pack's 7 is better), E02 stored at 9 (it is not).
	e := newPolicyEnv(t, map[int]int{1: 5, 2: 9})
	resp, err := e.m.Add(context.Background(), AddRequest{
		Type: "tv", Title: "Show", Hash: newHash, Season: 1, Replace: "if-better", QualityScore: 7, MetadataWait: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionReplaced || len(resp.Files) != 1 || resp.Files[0].Episode != 1 || resp.Files[0].FusePath == "" {
		t.Fatalf("response = %+v", resp)
	}
	if len(resp.Skipped) != 1 || resp.Skipped[0].Episode != 2 {
		t.Fatalf("skipped = %+v", resp.Skipped)
	}
	for _, f := range resp.Files {
		if f.Episode == 2 {
			t.Fatal("a refused episode is in files")
		}
	}
	if exists(e.oldStub[1]) || !exists(e.oldStub[2]) || exists(e.newStub(2)) || !exists(e.newStub(1)) {
		t.Fatal("stubs do not match the decision")
	}
	if e.reg.rows[EpisodeKey("Show", 1, 1)].Hash != newHash || e.reg.rows[EpisodeKey("Show", 1, 2)].Hash != oldHash {
		t.Fatalf("registry = %+v", e.reg.rows)
	}
	if len(*e.added) != 1 || len((*e.added)[0].files) != 1 {
		t.Fatalf("OnAdded = %+v: only the written episode may be announced", *e.added)
	}
	if n := countOf(e.eng.seen(), "RemoveTorrent"); n != 0 {
		t.Fatalf("RemoveTorrent calls = %d: the old release still has a kept episode, its torrent must stay", n)
	}
	if reads := countOf(e.eng.seen(), "ReadFileHead"); reads != 1 {
		t.Fatalf("reads = %d, want one gate read for the episode that replaces", reads)
	}
}

// A pack whose every episode is refused answers kept with an empty files, writes
// nothing and lets the torrent go.
func TestPackWithEveryEpisodeRefusedIsKept(t *testing.T) {
	e := newPolicyEnv(t, map[int]int{1: 5, 2: 5})
	resp, err := e.m.Add(context.Background(), AddRequest{
		Type: "tv", Title: "Show", Hash: newHash, Season: 1, Replace: "never", MetadataWait: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionKept || resp.Files == nil || len(resp.Files) != 0 || len(resp.Skipped) != 2 {
		t.Fatalf("response = %+v", resp)
	}
	if exists(e.newStub(1)) || exists(e.newStub(2)) || len(*e.added) != 0 {
		t.Fatal("a refused pack wrote or announced something")
	}
	if got := e.eng.seen(); !reflect.DeepEqual(got, []string{"AddTorrent", "GetTorrentInfo", "RemoveTorrent"}) {
		t.Fatalf("engine calls = %v, want add, metadata, then the drop", got)
	}
}

// Movies do not replace: a replace field changes nothing for them.
func TestMovieIgnoresTheReplacePolicy(t *testing.T) {
	e := newCharEnv(t, movieTorrent...)
	if _, err := e.add(AddRequest{Title: "Inception", Hash: charPackHash, Year: 2010}); err != nil {
		t.Fatal(err)
	}
	resp, err := e.add(AddRequest{Title: "Inception", Hash: charOtherHash, Year: 2010, Replace: "never"})
	if err != nil || resp.Decision != DecisionAdded || len(resp.Files) != 1 {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if got := mkvs(t, e.movies); len(got) != 2 {
		t.Fatalf("movies dir = %v, want two stubs", got)
	}
}

func TestUnknownReplaceValueIsABadRequest(t *testing.T) {
	e := newCharEnv(t, movieTorrent...)
	_, err := e.add(AddRequest{Title: "Inception", Hash: charPackHash, Replace: "sometimes"})
	if statusOf(t, err) != http.StatusBadRequest || !strings.Contains(err.Error(), "never, if-better or always") {
		t.Fatalf("err = %v", err)
	}
}

// The same release again is already_present whatever the policy says.
func TestAlreadyPresentBeatsThePolicy(t *testing.T) {
	e := newPolicyEnv(t, nil)
	if _, err := e.add(1, "", 3); err != nil {
		t.Fatal(err)
	}
	resp, err := e.add(1, "never", 3)
	if err != nil || !resp.AlreadyPresent || resp.Decision != DecisionAlreadyPresent {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}

// Only a request that names the replace field sees the new answer fields; the
// handler answers 200 for a kept add and 201 for one that wrote.
func TestLifecycleFieldsAppearOnlyForARequestThatOptsIn(t *testing.T) {
	post := func(e *policyEnv, body string) (int, map[string]json.RawMessage) {
		rec := httptest.NewRecorder()
		NewHandler(e.m).Add(rec, httptest.NewRequest(http.MethodPost, "/api/library/add", strings.NewReader(body)))
		var got map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%v: %s", err, rec.Body)
		}
		return rec.Code, got
	}
	base := `{"type":"tv","title":"Show","hash":"` + newHash + `","season":1,"episode":1,"metadata_wait":1`

	code, got := post(newPolicyEnv(t, map[int]int{1: 5}), base+`}`)
	if code != http.StatusCreated {
		t.Fatalf("plain replacing add: status %d", code)
	}
	for _, k := range []string{"decision", "replaced", "skipped"} {
		if _, ok := got[k]; ok {
			t.Errorf("plain add answered the new key %q", k)
		}
	}

	code, got = post(newPolicyEnv(t, map[int]int{1: 5}), base+`,"replace":"always"}`)
	if code != http.StatusCreated || string(got["decision"]) != `"replaced"` || len(got["replaced"]) == 0 {
		t.Fatalf("opted-in replacing add: status %d, %v", code, got)
	}

	code, got = post(newPolicyEnv(t, map[int]int{1: 5}), base+`,"replace":"never"}`)
	if code != http.StatusOK || string(got["decision"]) != `"kept"` || string(got["files"]) != `[]` {
		t.Fatalf("kept add: status %d, %v", code, got)
	}
}

// Two adds of the same episode with different policies are serialized by the episode
// lock: whichever runs second sees the first one's row. Under -race.
func TestConcurrentAddsWithDifferentPoliciesAreSerialized(t *testing.T) {
	for range 20 {
		e := newPolicyEnv(t, map[int]int{1: 5})
		var wg sync.WaitGroup
		var never, always *AddResponse
		var neverErr, alwaysErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			never, neverErr = e.m.Add(context.Background(), AddRequest{
				Type: "tv", Title: "Show", Hash: charOtherHash, Season: 1, Episode: 1, Replace: "never", MetadataWait: 1})
		}()
		go func() {
			defer wg.Done()
			always, alwaysErr = e.m.Add(context.Background(), AddRequest{
				Type: "tv", Title: "Show", Hash: newHash, Season: 1, Episode: 1, Replace: "always", MetadataWait: 1})
		}()
		wg.Wait()
		if neverErr != nil || alwaysErr != nil {
			t.Fatalf("errors: %v, %v", neverErr, alwaysErr)
		}
		if never.Decision != DecisionKept || always.Decision != DecisionReplaced {
			t.Fatalf("never = %q, always = %q", never.Decision, always.Decision)
		}
		if got := e.reg.rows[EpisodeKey("Show", 1, 1)].Hash; got != newHash {
			t.Fatalf("registry holds %s, want the always release", got)
		}
		if exists(e.oldStub[1]) {
			t.Fatal("old stub survived the replacement")
		}
	}
}

func countOf(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

// The policy decides first; the sampled readability read (first and last regular
// episode plus the replaced one) then runs only over the episodes that will replace.
// A refused episode is never read, even when it is the pack's first or last.
func TestSampledReadChoosesOnlyAmongTheReplacingEpisodes(t *testing.T) {
	// Stored scores: E02, E04, E05 beat the pack's 5 and are refused; E01 and E03 lose.
	e := newPolicyEnv(t, map[int]int{1: 1, 2: 9, 3: 1, 4: 9, 5: 9})
	e.eng.files = nil
	for ep := 1; ep <= 5; ep++ {
		e.eng.files = append(e.eng.files, FileStat{ID: ep + 10, Path: "Show.S01/Show.S01E0" + string(rune('0'+ep)) + ".mkv", Length: 1 << 30})
	}
	resp, err := e.m.Add(context.Background(), AddRequest{
		Type: "tv", Title: "Show", Hash: newHash, Season: 1, Replace: "if-better", QualityScore: 5, MetadataWait: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionReplaced || len(resp.Files) != 2 || len(resp.Skipped) != 3 {
		t.Fatalf("response = %+v", resp)
	}
	got := append([]int(nil), e.eng.readIDs...)
	sort.Ints(got)
	// E01 (replacing and first) and E03 (the last replacing episode): file ids 11 and 13.
	if !reflect.DeepEqual(got, []int{11, 13}) {
		t.Fatalf("read file ids = %v, want [11 13]: only replacing episodes may be sampled", got)
	}
}

// With no prior row, two adds of different releases of one episode, both never: the
// episode lock lets exactly one write; the other finds its row and is kept.
func TestConcurrentNeverAddsWithNoPriorWriteExactlyOne(t *testing.T) {
	for range 20 {
		e := newPolicyEnv(t, nil)
		var wg sync.WaitGroup
		res := make([]*AddResponse, 2)
		errs := make([]error, 2)
		for i, h := range []string{newHash, charOtherHash} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i], errs[i] = e.m.Add(context.Background(), AddRequest{
					Type: "tv", Title: "Show", Hash: h, Season: 1, Episode: 1, Replace: "never", MetadataWait: 1})
			}()
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("errors: %v, %v", errs[0], errs[1])
		}
		added, kept := 0, 0
		for _, r := range res {
			switch r.Decision {
			case DecisionAdded:
				added++
			case DecisionKept:
				kept++
			}
		}
		if added != 1 || kept != 1 {
			t.Fatalf("decisions %q and %q, want one added and one kept", res[0].Decision, res[1].Decision)
		}
		if got := mkvs(t, e.tv); len(got) != 1 {
			t.Fatalf("stubs = %v, want exactly one", got)
		}
	}
}

// A registry row whose stub is gone holds no release: never does not keep it.
func TestStaleRegistryRowCountsAsNoPrior(t *testing.T) {
	e := newPolicyEnv(t, map[int]int{1: 5})
	if err := os.Remove(e.oldStub[1]); err != nil {
		t.Fatal(err)
	}
	resp, err := e.add(1, "never", 0)
	if err != nil || resp.Decision != DecisionAdded || len(resp.Files) != 1 {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	if len(resp.Replaced) != 0 {
		t.Fatalf("replaced = %+v, want none: there was no stub", resp.Replaced)
	}
}

// A base32 request names the release by its hex hash for the stub name: a dry run of a
// release already filed is already_present, not a replacement of its own stub.
func TestBase32DryRunOfAFiledReleaseIsAlreadyPresent(t *testing.T) {
	e := newPolicyEnv(t, nil)
	if _, err := e.add(1, "", 0); err != nil {
		t.Fatal(err)
	}
	b32 := base32.StdEncoding.EncodeToString(mustHex(t, newHash))
	resp, err := e.m.Add(context.Background(), AddRequest{
		Type: "tv", Title: "Show", Hash: strings.ToLower(b32), Season: 1, Episode: 1, Replace: "never", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Decision != DecisionAlreadyPresent {
		t.Fatalf("decision = %q, want already_present: %+v", resp.Decision, resp)
	}
	// A base32 dry run of a release not yet filed plans the hex-tagged stub.
	e2 := newPolicyEnv(t, nil)
	resp, err = e2.m.Add(context.Background(), AddRequest{
		Type: "tv", Title: "Show", Hash: strings.ToLower(b32), Season: 1, Episode: 1, DryRun: true})
	if err != nil || resp.Decision != DecisionAdded || len(resp.Files) != 1 || resp.Files[0].Path != e2.newStub(1) {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
}

func mustHex(t *testing.T, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
