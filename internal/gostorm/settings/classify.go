package settings

import (
	"reflect"
	"sort"
)

// Class says what applying a changed field takes.
type Class int

const (
	// Hot fields are read where they are used (or when a torrent is activated), so a
	// store is enough: no torrent client is rebuilt and no torrent is dropped.
	Hot Class = iota
	// Client fields are consumed when the torrent client is built (BTServer.configure).
	// A change needs Disconnect and Connect, which drops every active torrent.
	Client
)

// fieldClass classifies every BTSets field from how the code consumes it, not from its
// name. TestEveryFieldIsClassified fails on a new field that is missing here; Diff treats
// an unknown field as Client, the safe side.
var fieldClass = map[string]Class{
	// read at use time or per activation
	"CacheSize":                Hot, // Storage capacity for caches opened from now on; active caches keep theirs
	"ReaderReadAHead":          Hot, // percentage read on every window and read-ahead calculation
	"PreloadCache":             Hot, // read when a preload starts
	"UseDisk":                  Hot, // read when a torrent is removed; the warmup dir is fixed at daemon start either way
	"TorrentsSavePath":         Hot, // same readers as UseDisk
	"RemoveCacheOnDrop":        Hot, // read when a cache closes
	"RetrackersMode":           Hot, // read when a torrent is added
	"TorrentDisconnectTimeout": Hot, // read when a torrent's idle timer is set
	"ResponsiveMode":           Hot, // getter read per piece
	"AdaptiveShield":           Hot, // getter read per piece
	// no consumer in the engine's client, or consumed only at daemon start (a rebuild
	// would not apply them either)
	"BlockListURL":        Hot, // only logged
	"SslPort":             Hot, // web server start
	"SslCert":             Hot, // web server start
	"SslKey":              Hot, // web server start
	"ShowFSActiveTorr":    Hot, // no consumer
	"StoreSettingsInJson": Hot, // storage preference, not engine
	// copied into the client configuration when it is built
	"ForceEncrypt":             Client, // header obfuscation and crypto policy
	"EnableDebug":              Client, // client logger level fixed at NewClient
	"EnableIPv6":               Client,
	"DisableTCP":               Client,
	"DisableUTP":               Client,
	"DisableUPNP":              Client,
	"DisableDHT":               Client,
	"DisablePEX":               Client,
	"DisableUpload":            Client,
	"DownloadRateLimit":        Client, // an unset limit is the engine's shared unlimited limiter: it cannot be tuned in place
	"UploadRateLimit":          Client, // same
	"ConnectionsLimit":         Client, // EstablishedConnsPerTorrent in the client config
	"AggressivePeerManagement": Client,
	"PeersListenPort":          Client,
}

// ClassOf returns the class of a field; an unknown field is Client.
func ClassOf(field string) Class {
	if c, ok := fieldClass[field]; ok {
		return c
	}
	return Client
}

// ClientFields lists the fields that need an engine restart, sorted.
func ClientFields() []string {
	var out []string
	for f, c := range fieldClass {
		if c == Client {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// Diff names the fields whose value differs between old and next, split by class.
func Diff(old, next BTSets) (hot, client []string) {
	ov, nv := reflect.ValueOf(old), reflect.ValueOf(next)
	t := ov.Type()
	for i := 0; i < t.NumField(); i++ {
		if reflect.DeepEqual(ov.Field(i).Interface(), nv.Field(i).Interface()) {
			continue
		}
		name := t.Field(i).Name
		if ClassOf(name) == Hot {
			hot = append(hot, name)
		} else {
			client = append(client, name)
		}
	}
	return hot, client
}
