package library

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestListReportsLinkedTrueFalseAndNull(t *testing.T) {
	e := newCharEnv(t)
	linked := auditStub(t, e, e.movies, "linked.mkv", auditHashA, 1, 10)
	_ = auditStub(t, e, e.movies, "alone.mkv", auditHashA, 2, 10)
	_ = auditStub(t, e, e.tv, "ep.mkv", auditHashB, 1, 10)

	byName := func(kind string) map[string]*bool {
		items, err := e.m.List(kind)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*bool{}
		for _, it := range items {
			out[filepath.Base(it.Path)] = it.Linked
		}
		return out
	}

	// No Linked source, then no set: null, and no count.
	if got := byName("movies"); got["linked.mkv"] != nil || got["alone.mkv"] != nil {
		t.Fatalf("no source: linked = %v, want null", got)
	}
	var set map[string]struct{}
	e.m.cfg.Linked = func() map[string]struct{} { return set }
	if got := byName("movies"); got["linked.mkv"] != nil {
		t.Fatalf("no set: linked = %v, want null", got)
	}
	byName("tv")
	if _, ok := e.m.UnlinkedStubs(); ok {
		t.Fatal("a count was reported with no linked set")
	}

	// A set: true for the named stub, false for the rest.
	set = map[string]struct{}{filepath.Base(linked): {}}
	got := byName("movies")
	if got["linked.mkv"] == nil || !*got["linked.mkv"] || got["alone.mkv"] == nil || *got["alone.mkv"] {
		t.Fatalf("with a set: linked = %v, want true and false", got)
	}
	// An empty set is still a set: everything is unlinked.
	set = map[string]struct{}{}
	if got := byName("tv"); got["ep.mkv"] == nil || *got["ep.mkv"] {
		t.Fatalf("empty set: tv linked = %v, want false", got)
	}
}

func TestListLinkedWireFormAndUnlinkedGauge(t *testing.T) {
	e := newCharEnv(t)
	linked := auditStub(t, e, e.movies, "linked.mkv", auditHashA, 1, 10)
	_ = auditStub(t, e, e.movies, "alone.mkv", auditHashA, 2, 10)
	_ = auditStub(t, e, e.tv, "ep.mkv", auditHashB, 1, 10)
	set := map[string]struct{}{filepath.Base(linked): {}}
	e.m.cfg.Linked = func() map[string]struct{} { return set }

	if _, ok := e.m.UnlinkedStubs(); ok {
		t.Fatal("a count before any list")
	}
	items, err := e.m.List("movies")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.m.UnlinkedStubs(); ok {
		t.Fatal("a count with only one kind listed")
	}
	if _, err := e.m.List("tv"); err != nil {
		t.Fatal(err)
	}
	if n, ok := e.m.UnlinkedStubs(); !ok || n != 2 { // alone.mkv and ep.mkv
		t.Fatalf("unlinked = %d, %v, want 2", n, ok)
	}

	b, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, ok := r["linked"].(bool); !ok {
			t.Errorf("linked = %v, want a bool", r["linked"])
		}
	}

	// The set goes away: the field is null on the wire and the count is withdrawn.
	set = nil
	items, _ = e.m.List("movies")
	b, _ = json.Marshal(items)
	_ = json.Unmarshal(b, &rows)
	for _, r := range rows {
		if v, present := r["linked"]; !present || v != nil {
			t.Errorf("no set: linked = %v (present %v), want null", v, present)
		}
	}
	if _, ok := e.m.UnlinkedStubs(); ok {
		t.Fatal("a count after the set went away")
	}
}
