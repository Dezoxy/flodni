module github.com/anacrolix/torrent

go 1.26.0

require (
	github.com/RoaringBitmap/roaring/v2 v2.30.0
	github.com/ajwerner/btree v0.3.0
	github.com/alexflint/go-arg v1.6.1
	github.com/anacrolix/chansync v0.8.0
	github.com/anacrolix/dht/v2 v2.24.0
	github.com/anacrolix/envpprof v1.6.0
	github.com/anacrolix/fuse v0.3.2
	github.com/anacrolix/generics v0.2.0
	github.com/anacrolix/go-libutp v1.5.1
	github.com/anacrolix/log v0.17.0
	github.com/anacrolix/missinggo v1.3.0
	github.com/anacrolix/missinggo/perf v1.0.0
	github.com/anacrolix/missinggo/v2 v2.11.0
	github.com/anacrolix/multiless v0.4.0
	github.com/anacrolix/possum/go v0.4.0
	github.com/anacrolix/squirrel v0.6.4
	github.com/anacrolix/sync v0.6.0
	github.com/anacrolix/tagflag v1.4.0
	github.com/anacrolix/upnp v0.1.4
	github.com/anacrolix/utp v0.2.0
	github.com/bahlo/generic-list-go v0.2.0
	github.com/cespare/xxhash v1.1.0
	github.com/davecgh/go-spew v1.1.1
	github.com/dustin/go-humanize v1.1.0
	github.com/elliotchance/orderedmap v1.8.0
	github.com/frankban/quicktest v1.14.6
	github.com/fsnotify/fsnotify v1.10.1
	github.com/go-llsqlite/adapter v0.2.0
	github.com/google/btree v1.1.3
	github.com/gorilla/websocket v1.5.3
	github.com/pion/datachannel v1.6.4
	github.com/pion/logging v0.2.4
	github.com/pion/webrtc/v4 v4.2.26
	github.com/pkg/errors v0.9.1
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	golang.org/x/exp v0.0.0-20261113113407-e3970619a1a4
	golang.org/x/sys v0.50.0
	golang.org/x/time v0.16.0
)

require (
	github.com/alecthomas/atomic v0.1.0-alpha2 // indirect
	github.com/alexflint/go-scalar v1.2.0 // indirect
	github.com/anacrolix/mmsg v1.0.1 // indirect
	github.com/anacrolix/stm v0.5.0 // indirect
	github.com/benbjohnson/immutable v0.4.1-0.20221220213129-8932b999621d // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/bradfitz/iter v0.0.0-20191230175014-e8f45d346db8 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/felixge/fgprof v0.9.5 // indirect
	github.com/go-llsqlite/crawshaw v0.4.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/pprof v0.0.0-20240227163752-401108e1b7e7 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/huandu/xstrings v1.3.2 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.16 // indirect
	github.com/mschoch/smat v0.2.0 // indirect
	github.com/pion/dtls/v3 v3.1.15 // indirect
	github.com/pion/ice/v4 v4.4.8 // indirect
	github.com/pion/interceptor v0.1.51 // indirect
	github.com/pion/mdns/v2 v2.2.2 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.20 // indirect
	github.com/pion/rtp v1.10.6 // indirect
	github.com/pion/sctp v1.12.1 // indirect
	github.com/pion/sdp/v3 v3.0.20 // indirect
	github.com/pion/srtp/v3 v3.1.3 // indirect
	github.com/pion/stun/v4 v4.0.1 // indirect
	github.com/pion/transport/v5 v5.1.4 // indirect
	github.com/pion/turn/v5 v5.1.3 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/rs/dnscache v0.0.0-20211102005908-e0241e321417 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	golang.org/x/crypto v0.59.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/xerrors v0.0.0-20220609144429-65e65417b02f // indirect
	modernc.org/libc v1.22.3 // indirect
	modernc.org/mathutil v1.5.0 // indirect
	modernc.org/memory v1.5.0 // indirect
	modernc.org/sqlite v1.21.1 // indirect
	zombiezen.com/go/sqlite v0.13.1 // indirect
)

retract (
	// Doesn't signal interest to peers if choked when piece priorities change.
	v1.39.0
	// peer-requesting doesn't scale
	[v1.34.0, v1.38.1]
	// Indefinite outgoing requests on storage write errors. https://github.com/anacrolix/torrent/issues/889
	[v1.29.0, v1.53.2]
)
