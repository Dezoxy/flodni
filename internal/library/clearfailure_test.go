package library

import (
	"context"
	"reflect"
	"testing"
)

// clearingRegistry is a memRegistry that also takes the optional method the manager
// asserts on the registry, so the assertion is exercised through Remove.
type clearingRegistry struct {
	*memRegistry
	cleared []string
}

func (r *clearingRegistry) ClearMetadataFailure(hash string) error {
	r.cleared = append(r.cleared, hash)
	return nil
}

func TestRemoveClearsTheReadabilityRecord(t *testing.T) {
	e := newCharEnv(t, movieTorrent...)
	reg := &clearingRegistry{memRegistry: e.reg}
	e.m.cfg.Registry = reg
	if _, err := e.add(AddRequest{Title: "Inception", Hash: charPackHash, Year: 2010}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Remove(context.Background(), RemoveRequest{Hash: charPackHash}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reg.cleared, []string{charPackHash}) {
		t.Fatalf("cleared = %v, want the removed hash", reg.cleared)
	}
}

// orderedRegistry records the calls the daemon's session close depends on in one
// sequence: the stub is invalidated (the session closes there) before the torrent's
// readability row is cleared, or a session would write the row back afterwards.
type orderedRegistry struct {
	*memRegistry
	calls *[]string
}

func (r orderedRegistry) ClearMetadataFailure(hash string) error {
	*r.calls = append(*r.calls, "clear "+hash)
	return nil
}

func TestReplacedStubIsInvalidatedBeforeItsRowIsCleared(t *testing.T) {
	eng := &replaceEngine{}
	m, oldStub, reg := replaceFixture(t, eng)
	var calls []string
	m.cfg.Registry = orderedRegistry{memRegistry: reg, calls: &calls}
	m.cfg.InvalidatePath = func(p string) { calls = append(calls, "invalidate "+p) }

	if _, err := addPack(m); err != nil {
		t.Fatal(err)
	}
	want := []string{"invalidate " + oldStub, "clear " + oldHash}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}
