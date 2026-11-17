package torrent

import (
	"errors"
	"math"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/log"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
)

// Malformed input from a peer must be refused, never crash the process or reach storage
// (engine hardening, adapted from upstream Tiramisu fd976eb).

func newMalformedTestTorrent(t *testing.T, info metainfo.Info) (*Client, *Torrent) {
	t.Helper()
	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableTrackers = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	b, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	tor, _, err := cl.AddTorrentSpec(&TorrentSpec{InfoHash: metainfo.HashBytes(b), InfoBytes: b})
	if err != nil {
		t.Fatal(err)
	}
	return cl, tor
}

func metadataMsg(t *testing.T, typ pp.ExtendedMetadataRequestMsgType, piece int) []byte {
	t.Helper()
	b, err := bencode.Marshal(pp.ExtendedMetadataRequestMsg{Type: typ, Piece: piece})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A negative piece index in a ut_metadata message used to index metadataRequests or
// metadataCompletedChunks with it.
func TestMalformedMetadataNegativePieceIndex(t *testing.T) {
	// Without info: the Request path indexed metadataCompletedChunks[-1].
	noInfo := &Torrent{logger: log.Default}
	// With info: the Request path sliced metadataBytes at a negative start.
	withInfo := &Torrent{info: &metainfo.Info{}, metadataBytes: make([]byte, 1<<15)}
	for _, tc := range []struct {
		name string
		tor  *Torrent
		typ  pp.ExtendedMetadataRequestMsgType
	}{
		{"data", noInfo, pp.DataMetadataExtensionMsgType},
		{"request without info", noInfo, pp.RequestMetadataExtensionMsgType},
		{"request with info", withInfo, pp.RequestMetadataExtensionMsgType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &PeerConn{Peer: Peer{t: tc.tor}}
			err := (&Client{}).gotMetadataExtensionMsg(metadataMsg(t, tc.typ, -1), tc.tor, c)
			if err == nil {
				t.Fatal("a negative metadata piece index was accepted")
			}
		})
	}
	c := &PeerConn{}
	if c.requestedMetadataPiece(-1) {
		t.Fatal("a negative index counts as requested")
	}
	noInfo.saveMetadataPiece(-1, []byte{1}) // must not panic
}

// (1<<14)*piece wraps for a large peer-supplied index, so the size check must not multiply.
func TestMalformedMetadataHugePieceIndexIsNotHeld(t *testing.T) {
	tor := &Torrent{info: &metainfo.Info{}, metadataBytes: make([]byte, 1<<15)}
	if !tor.haveMetadataPiece(1) {
		t.Fatal("setup: piece 1 of 2 is not held")
	}
	for _, piece := range []int{2, 1 << 49, math.MaxInt} {
		if tor.haveMetadataPiece(piece) {
			t.Fatalf("metadata piece %d of a 2-piece metadata is reported held", piece)
		}
	}
}

func TestMalformedInfoIsRefused(t *testing.T) {
	pieces := func(n int) []byte { return make([]byte, 20*n) }
	for _, tc := range []struct {
		name string
		info metainfo.Info
	}{
		// Zero length and one hash: the piece-count check below alone accepts this.
		{"negative piece length", metainfo.Info{Name: "x", Length: 0, PieceLength: -16384, Pieces: pieces(1)}},
		{"oversized piece length", metainfo.Info{Name: "x", Length: 1 << 20, PieceLength: 1 << 40, Pieces: pieces(1)}},
		{"zero piece length with hashes", metainfo.Info{Name: "x", Length: 0, PieceLength: 0, Pieces: pieces(1)}},
		{"negative file length", metainfo.Info{Name: "x", PieceLength: 16384, Pieces: pieces(1), Files: []metainfo.FileInfo{
			{Length: 16384, Path: []string{"a"}}, {Length: -1, Path: []string{"b"}},
		}}},
		// The sum wraps to -2, which the piece-count check below alone accepts for no pieces.
		{"file lengths overflow", metainfo.Info{Name: "x", PieceLength: 16384, Files: []metainfo.FileInfo{
			{Length: math.MaxInt64, Path: []string{"a"}}, {Length: math.MaxInt64, Path: []string{"b"}},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateInfo(&tc.info); err == nil {
				t.Fatal("malformed info accepted")
			}
		})
	}
	ok := metainfo.Info{Name: "x", Length: 16384, PieceLength: 16384, Pieces: pieces(1)}
	if err := validateInfo(&ok); err != nil {
		t.Fatalf("well-formed info refused: %v", err)
	}
}

func TestMalformedChunkMustMatchExpectedRequest(t *testing.T) {
	// Two 32 KiB pieces; the second is short: one full chunk and a 100-byte tail.
	cl, tor := newMalformedTestTorrent(t, metainfo.Info{
		Name: "chunks", Length: 32768 + 16384 + 100, PieceLength: 32768, Pieces: make([]byte, 40),
	})
	cl.lock()
	defer cl.unlock()
	for _, tc := range []struct {
		name string
		r    Request
		ok   bool
	}{
		{"first chunk", chunkReq(0, 0, 16384), true},
		{"second chunk", chunkReq(0, 16384, 16384), true},
		{"short tail chunk", chunkReq(1, 16384, 100), true},
		{"misaligned begin", chunkReq(0, 1, 16384), false},
		{"begin not on a chunk boundary", chunkReq(0, 16383, 1), false},
		{"shorter than the chunk", chunkReq(0, 0, 8192), false},
		{"longer than the chunk", chunkReq(0, 0, 16385), false},
		{"runs past the piece", chunkReq(0, 16384, 32768), false},
		{"tail chunk too long", chunkReq(1, 16384, 16384), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tor.checkValidReceiveChunk(tc.r)
			if tc.ok && err != nil {
				t.Fatalf("expected chunk refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("malformed chunk accepted")
			}
		})
	}
}

func chunkReq(index, begin, length int) Request {
	return Request{pp.Integer(index), ChunkSpec{Begin: pp.Integer(begin), Length: pp.Integer(length)}}
}

type pingCounter struct {
	DhtServer
	pings atomic.Int32
}

func (p *pingCounter) Ping(*net.UDPAddr) { p.pings.Add(1) }

func (p *pingCounter) AddNode(krpc.NodeInfo) error { return nil }

func TestMalformedPortMessageOnlyFirstPings(t *testing.T) {
	dht := &pingCounter{}
	cl := &Client{dhtServers: []DhtServer{dht}}
	c := &PeerConn{Peer: Peer{t: &Torrent{cl: cl}, RemoteAddr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 6881}}}
	for port := uint16(1); port <= 50; port++ {
		c.onPeerSentPort(port)
	}
	time.Sleep(100 * time.Millisecond) // pings are sent from goroutines
	if n := dht.pings.Load(); n != 1 {
		t.Fatalf("%d DHT pings for 50 Port messages on one connection, want 1", n)
	}
}

func TestMalformedUnsupportedNetworkErrors(t *testing.T) {
	wrap := func(call string, errno syscall.Errno) error {
		return &net.OpError{Op: "listen", Err: os.NewSyscallError(call, errno)}
	}
	for _, tc := range []struct {
		name string
		err  error
		skip bool
	}{
		{"no IPv6 in the kernel", wrap("socket", syscall.EAFNOSUPPORT), true},
		{"no address to bind", wrap("bind", syscall.EADDRNOTAVAIL), true},
		{"port in use", wrap("bind", syscall.EADDRINUSE), false},
		{"permission denied", wrap("bind", syscall.EACCES), false},
		{"unrelated", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnsupportedNetworkError(tc.err); got != tc.skip {
				t.Fatalf("isUnsupportedNetworkError = %v, want %v", got, tc.skip)
			}
		})
	}
}

// A peer-supplied metadata_size must be bounded on the wide type: uint32(-4294967291) is 5.
func TestMalformedMetadataSizeIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		ok   bool
	}{
		{"negative that truncates to a small size", -4294967291, false},
		{"negative", -1, false},
		{"above 32 bits", 1<<32 + 5, false},
		{"just over the maximum", int(maxMetadataSize) + 1, false},
		{"valid", 1 << 15, true},
		{"the maximum", int(maxMetadataSize), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tor := &Torrent{logger: log.Default}
			err := tor.setMetadataSize(tc.size)
			if tc.ok && (err != nil || len(tor.metadataBytes) != tc.size) {
				t.Fatalf("size %d refused or not applied: err=%v len=%d", tc.size, err, len(tor.metadataBytes))
			}
			if !tc.ok && (err == nil || len(tor.metadataBytes) != 0) {
				t.Fatalf("size %d accepted: err=%v len=%d", tc.size, err, len(tor.metadataBytes))
			}
		})
	}
}

// An unparsable webseed URL is dropped once, when added, not retried by every request.
func TestMalformedWebseedURLIsDroppedWhenAdded(t *testing.T) {
	cl, tor := newMalformedTestTorrent(t, metainfo.Info{
		Name: "ws", Length: 16384, PieceLength: 16384, Pieces: make([]byte, 20),
	})
	cl.lock()
	defer cl.unlock()
	tor.addWebSeed("http://bad host/")
	if n := len(tor.webSeeds); n != 0 {
		t.Fatalf("%d webseed(s) kept for an unparsable URL", n)
	}
	tor.addWebSeed("http://127.0.0.1:1/")
	if n := len(tor.webSeeds); n != 1 {
		t.Fatalf("a good webseed URL was dropped: %d kept", n)
	}
}
