package library

import (
	"net/http"
	"sort"
)

// auditCap bounds each array in the audit answer; the totals stay exact.
const auditCap = 200

// Reasons an audited stub is reported.
const (
	// AuditNoSuchFile: the stub's index names no file in the engine's stored list.
	AuditNoSuchFile = "no_such_file"
	// AuditLengthDiffers: the index names a file of another length than the stub's size.
	AuditLengthDiffers = "length_differs"
)

// AuditMismatch is one stub whose index does not name the file it was written for.
type AuditMismatch struct {
	Path     string `json:"path"`
	Hash     string `json:"hash"`
	Index    int    `json:"index"`
	StubSize int64  `json:"stub_size"`
	// EngineLength is null when the index names no file.
	EngineLength *int64 `json:"engine_length"`
	Reason       string `json:"reason"`
}

// AuditNoFileList counts the stubs whose torrent record holds no file list. One wake
// reads it; the audit wakes nothing.
type AuditNoFileList struct {
	Stubs    int      `json:"stubs"`
	Torrents int      `json:"torrents"`
	Hashes   []string `json:"hashes"` // capped, sorted
}

// AuditReport is the answer of GET /api/library/audit.
type AuditReport struct {
	Checked         int             `json:"checked"` // stubs compared against a stored list
	MismatchedTotal int             `json:"mismatched_total"`
	Truncated       bool            `json:"truncated"`
	Mismatched      []AuditMismatch `json:"mismatched"`
	NoFileList      AuditNoFileList `json:"no_file_list"`
	// Unreadable counts stubs that carry no readable hash; they cannot be checked.
	Unreadable int `json:"unreadable"`
}

// Audit compares every stub's index with the file list in the engine's stored record
// (Config.StoredFiles, engine ids). Read-only: it changes no stub, record or state and
// wakes no torrent. It reuses List, so it sees the same stubs the list route does.
func (m *Manager) Audit() (*AuditReport, error) {
	if m.cfg.StoredFiles == nil {
		return nil, errf(http.StatusServiceUnavailable, "stored records are not available")
	}
	rep := &AuditReport{Mismatched: []AuditMismatch{}, NoFileList: AuditNoFileList{Hashes: []string{}}}
	lists := map[string][]TorrentFile{} // one record read per torrent
	noList := map[string]bool{}
	for _, kind := range []string{"movie", "tv"} {
		items, err := m.List(kind)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Hash == "" {
				rep.Unreadable++
				continue
			}
			files, ok := lists[it.Hash]
			if !ok {
				files = m.cfg.StoredFiles(it.Hash)
				lists[it.Hash] = files
			}
			if len(files) == 0 {
				rep.NoFileList.Stubs++
				noList[it.Hash] = true
				continue
			}
			rep.Checked++
			mm := AuditMismatch{Path: it.Path, Hash: it.Hash, Index: it.FileIndex, StubSize: it.Size, Reason: AuditNoSuchFile}
			for _, f := range files {
				if f.Index != it.FileIndex {
					continue
				}
				if f.Length == it.Size {
					mm.Reason = ""
					break
				}
				l := f.Length
				mm.EngineLength, mm.Reason = &l, AuditLengthDiffers
				break
			}
			if mm.Reason == "" {
				continue
			}
			rep.MismatchedTotal++
			if len(rep.Mismatched) < auditCap {
				rep.Mismatched = append(rep.Mismatched, mm)
			}
		}
	}
	rep.Truncated = rep.MismatchedTotal > len(rep.Mismatched)
	rep.NoFileList.Torrents = len(noList)
	for h := range noList {
		rep.NoFileList.Hashes = append(rep.NoFileList.Hashes, h)
	}
	sort.Strings(rep.NoFileList.Hashes)
	if len(rep.NoFileList.Hashes) > auditCap {
		rep.NoFileList.Hashes = rep.NoFileList.Hashes[:auditCap]
	}
	sort.Slice(rep.Mismatched, func(i, j int) bool { return rep.Mismatched[i].Path < rep.Mismatched[j].Path })
	return rep, nil
}
