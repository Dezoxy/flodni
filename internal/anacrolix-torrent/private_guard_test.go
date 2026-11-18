package torrent

import (
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
)

// A private torrent (BEP 27) must stay off the DHT and out of PEX even when both
// are on for public torrents: its peers are offered no ut_pex.
func TestPrivateTorrentsGetNoPex(t *testing.T) {
	yes, no := true, false
	cl := &Client{
		defaultLocalLtepProtocolMap: makeBuiltinLtepProtocols(true),
		privateLocalLtepProtocolMap: makeBuiltinLtepProtocols(false),
	}
	cases := []struct {
		name    string
		info    *metainfo.Info
		private bool
	}{
		{"magnet without metadata yet", nil, false},
		{"public", &metainfo.Info{Private: &no}, false},
		{"no flag", &metainfo.Info{}, false},
		{"private", &metainfo.Info{Private: &yes}, true},
	}
	for _, c := range cases {
		tor := &Torrent{cl: cl, info: c.info}
		if got := tor.isPrivate(); got != c.private {
			t.Errorf("%s: isPrivate() = %v, want %v", c.name, got, c.private)
		}
		pc := &PeerConn{}
		pc.t = tor
		pc.addBuiltinLtepProtocols(true)
		offersPex := false
		for _, name := range pc.LocalLtepProtocolMap.Index {
			if name == pp.ExtensionNamePex {
				offersPex = true
			}
		}
		if offersPex == c.private {
			t.Errorf("%s: offers ut_pex = %v", c.name, offersPex)
		}
	}
}

// fakeDht counts announces and never returns peers.
type fakeDht struct {
	announces atomic.Int32
}

func (f *fakeDht) Stats() interface{}          { return nil }
func (f *fakeDht) ID() (id [20]byte)           { return }
func (f *fakeDht) Addr() net.Addr              { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6881} }
func (f *fakeDht) AddNode(krpc.NodeInfo) error { return nil }
func (f *fakeDht) Ping(*net.UDPAddr)           {}
func (f *fakeDht) WriteStatus(io.Writer)       {}
func (f *fakeDht) Announce([20]byte, int, bool) (DhtAnnounce, error) {
	f.announces.Add(1)
	return fakeAnnounce{}, nil
}

type fakeAnnounce struct{}

func (fakeAnnounce) Close() {}
func (fakeAnnounce) Peers() <-chan dht.PeersValues {
	c := make(chan dht.PeersValues)
	close(c)
	return c
}

// A private torrent added with its metadata (the .torrent upload path) must
// never reach the DHT: the announcer is started when the torrent is created,
// and used to win the client lock against MergeSpec setting the info, so one
// announce could go out before the torrent was known to be private.
func TestPrivateAddSpecNeverAnnouncesToDht(t *testing.T) {
	cfg := NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cfg.DisableTrackers = true
	cfg.DisableIPv6 = true
	cfg.NoDHT = true
	cfg.PeriodicallyAnnounceTorrentsToDht = true
	cl, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	fd := &fakeDht{}
	cl.AddDhtServer(fd)

	yes := true
	const rounds = 300
	for i := 0; i < rounds; i++ {
		info := metainfo.Info{
			Name:        fmt.Sprintf("private-%d", i),
			Length:      16384,
			PieceLength: 16384,
			Pieces:      make([]byte, 20),
			Private:     &yes,
		}
		b, err := bencode.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		spec := &TorrentSpec{InfoHash: metainfo.HashBytes(b), InfoBytes: b}
		if _, _, err := cl.AddTorrentSpec(spec); err != nil {
			t.Fatal(err)
		}
	}
	// Let any announcer goroutine that is going to run, run.
	time.Sleep(200 * time.Millisecond)
	if n := fd.announces.Load(); n != 0 {
		t.Errorf("%d private torrents announced to the DHT of %d", n, rounds)
	}
}

// BEP 27 / hard rule 3: a private torrent's peer must not make us talk to the DHT. A Port
// message would otherwise ping the DHT node at an address the peer names.
func TestPrivatePeerPortMessageNeverPingsDht(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		name    string
		info    *metainfo.Info
		private bool
	}{
		{"private", &metainfo.Info{Private: &yes}, true},
		{"public control", &metainfo.Info{}, false},
	} {
		dht := &pingCounter{}
		cl := &Client{dhtServers: []DhtServer{dht}}
		c := &PeerConn{Peer: Peer{t: &Torrent{cl: cl, info: tc.info}, RemoteAddr: &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 6881}}}
		c.onPeerSentPort(6881)
		time.Sleep(100 * time.Millisecond) // pings are sent from goroutines
		want := int32(1)
		if tc.private {
			want = 0
		}
		if n := dht.pings.Load(); n != want {
			t.Errorf("%s: %d DHT pings for a Port message, want %d", tc.name, n, want)
		}
	}
}
