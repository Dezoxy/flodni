package torrent

import (
	"sort"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// refillPeer is a stand-in peer that holds everything and records the wire calls the engine
// makes to it. Not a real connection: the request-refill tests drive getDesiredRequestState and
// applyRequestState directly, under the client lock.
type refillPeer struct {
	peerImpl
	requested []RequestIndex
	cancelled []RequestIndex
	t         *Torrent
}

func (r *refillPeer) handleUpdateRequests()               {}
func (r *refillPeer) writeInterested(bool) bool           { return true }
func (r *refillPeer) _cancel(RequestIndex) bool           { return false }
func (r *refillPeer) connectionFlags() string             { return "" }
func (r *refillPeer) peerHasAllPieces() (all, known bool) { return true, true }
func (r *refillPeer) peerPieces() *roaring.Bitmap         { return roaring.New() }
func (r *refillPeer) onClose()                            {}
func (r *refillPeer) String() string                      { return "refill-peer" }
func (r *refillPeer) _request(req Request) bool {
	r.requested = append(r.requested, r.t.requestIndexFromRequest(req))
	return true
}

// refillRig is a torrent of pieces*chunksPerPiece chunks with every piece wanted and no data.
type refillRig struct {
	cl  *Client
	tor *Torrent
}

const refillChunk = 16384

// newRefillRig builds the rig. tweak may change the client config before the client starts.
func newRefillRig(t *testing.T, pieces, chunksPerPiece int, tweak func(*ClientConfig)) *refillRig {
	t.Helper()
	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableTrackers = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	// A steal is judged on queue sizes and the stall rule here, not on the grace.
	cfg.StealRequestGrace = 0
	if tweak != nil {
		tweak(cfg)
	}
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	b, err := bencode.Marshal(metainfo.Info{
		Name:        "refill",
		Length:      int64(refillChunk * chunksPerPiece * pieces),
		PieceLength: int64(refillChunk * chunksPerPiece),
		Pieces:      make([]byte, 20*pieces),
	})
	if err != nil {
		t.Fatal(err)
	}
	tor, _, err := cl.AddTorrentSpec(&TorrentSpec{InfoHash: metainfo.HashBytes(b), InfoBytes: b})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		cl.lock()
		busy := false
		for i := 0; i < pieces; i++ {
			if tor.pieces[i].hashing || tor.pieces[i].queuedForHash() {
				busy = true
			}
		}
		cl.unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pieces never finished hashing")
		}
	}
	prios := map[int]piecePriority{}
	for i := 0; i < pieces; i++ {
		prios[i] = PiecePriorityNormal
	}
	tor.SetPiecePriorities(prios)
	return &refillRig{cl: cl, tor: tor}
}

// addPeer registers a stand-in peer. Call before taking the client lock.
func (g *refillRig) addPeer(name string) (*Peer, *refillPeer) {
	impl := &refillPeer{t: g.tor}
	p := &Peer{t: g.tor, peerImpl: impl, callbacks: &Callbacks{}}
	p.requestState.Requests = &peerRequests{}
	p.PeerMaxRequests = 250
	g.cl.lock()
	if g.tor.webSeeds == nil {
		g.tor.webSeeds = map[string]*Peer{}
	}
	g.tor.webSeeds[name] = p
	g.cl.unlock()
	return p, impl
}

// refill runs one request update for p as the writer does: build the desired state, apply it.
func (g *refillRig) refill(p *Peer, reason string) (candidates int) {
	g.cl.lock()
	defer g.cl.unlock()
	p.needRequestUpdate = reason
	next := p.getDesiredRequestState()
	candidates = len(next.Requests.requestIndexes)
	p.applyRequestState(next)
	return
}

func (g *refillRig) held(p *Peer) []RequestIndex {
	g.cl.lock()
	defer g.cl.unlock()
	var out []RequestIndex
	p.requestState.Requests.Iterate(func(r RequestIndex) bool { out = append(out, r); return true })
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// forceHold makes p hold r as if it had asked for it, for the setup of a scenario.
func (g *refillRig) forceHold(p *Peer, reqs ...RequestIndex) {
	g.cl.lock()
	defer g.cl.unlock()
	p.requestState.Interested = true
	for _, r := range reqs {
		p.requestState.Requests.Add(r)
		g.tor.requestState[r] = requestState{peer: p, when: time.Now().Add(-time.Minute)}
	}
	p._chunksReceivedWhileExpecting = 100000 // a warmed peer: the queue target is not the 2-chunk floor
	p.updateExpectingChunks()
}
