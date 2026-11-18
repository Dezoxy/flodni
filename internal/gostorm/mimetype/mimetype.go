// Package mimetype names the type of a streamed file from its extension alone.
//
// The file name comes from the swarm, so the answer must not depend on the host (Go's
// mime package reads /etc/mime.types, which differs between images) and must never open a
// file on the daemon's disk. Only the video and audio types below are known; any other
// extension, playlists included, is not media here.
package mimetype

import (
	"path"
	"strings"
)

// byExt maps a lower-case extension to the type served inline. The DLNA-compat choices
// (mpeg for transport streams, avi for AVI) are the ones players were tuned against.
var byExt = map[string]string{
	// video
	".mkv": "video/x-matroska", ".mpv": "video/x-matroska",
	".mp4": "video/mp4", ".m4v": "video/mp4",
	".webm": "video/webm",
	".avi":  "video/avi",
	".mpeg": "video/mpeg", ".mpg": "video/mpeg", ".mpe": "video/mpeg",
	".ts": "video/mpeg", ".m2ts": "video/mpeg", ".mts": "video/mpeg",
	".mov": "video/x-quicktime", ".qt": "video/x-quicktime",
	".ogv": "video/ogg",
	".vob": "video/x-ms-vob",
	".flv": "video/x-flv",
	".wmv": "video/x-ms-wmv", ".asf": "video/x-ms-asf",
	".3gp": "video/3gpp", ".3g2": "video/3gpp2",
	".dv": "video/dv", ".dif": "video/dv",
	".fli":  "video/fli",
	".rmvb": "application/vnd.rn-realmedia-vbr",
	// audio
	".mp3": "audio/mpeg", ".mp2": "audio/mpeg", ".mpga": "audio/mpeg", ".mpega": "audio/mpeg",
	".m4a": "audio/mp4", ".m4b": "audio/mp4",
	".aac":  "audio/x-aac",
	".flac": "audio/flac",
	".ogg":  "audio/ogg", ".oga": "audio/ogg", ".spx": "audio/ogg",
	".opus": "audio/opus",
	".weba": "audio/weba",
	".wav":  "audio/x-wav",
	".ape":  "audio/x-ape",
	".dsd":  "audio/dsd", ".dsf": "audio/dsd", ".dff": "audio/dsd",
	".mka": "audio/x-matroska",
	".ac3": "audio/ac3",
	".dts": "audio/vnd.dts",
	".wma": "audio/x-ms-wma",
}

// StreamType returns the video or audio type for a file path, and false for anything
// else. ".part" is ignored.
func StreamType(filePath string) (string, bool) {
	name := strings.TrimSuffix(path.Base(strings.ReplaceAll(filePath, "\\", "/")), ".part")
	t, ok := byExt[strings.ToLower(path.Ext(name))]
	return t, ok
}
