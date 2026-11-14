package library

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxBodyBytes = 1 << 20

// Handler exposes the manager over HTTP: POST /api/library/add,
// POST /api/library/remove and GET /api/library/list. It exists so a client with no
// access to the filesystem can still file a title into the library.
type Handler struct {
	mgr *Manager
}

func NewHandler(m *Manager) *Handler { return &Handler{mgr: m} }

func (h *Handler) Add(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req AddRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.mgr.Add(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	status := http.StatusCreated
	if resp.AlreadyPresent {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}

func (h *Handler) Remove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req RemoveRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.mgr.Remove(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Inspect reports a torrent's source files so a caller can map them to virtual
// paths before adding them.
func (h *Handler) Inspect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req InspectRequest
	if err := decode(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := h.mgr.Inspect(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	// type=gaps reuses this endpoint rather than adding one: the response stays an
	// array, so a client that asks for movies or tv sees no change.
	if strings.EqualFold(r.URL.Query().Get("type"), "gaps") {
		gaps, total, err := h.mgr.ListGaps()
		if err != nil {
			writeAPIError(w, err)
			return
		}
		// The body stays an array, so the count travels in a header: without it a
		// client reading a capped page cannot tell there is more behind it.
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		writeJSON(w, http.StatusOK, gaps)
		return
	}
	items, err := h.mgr.List(r.URL.Query().Get("type"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// Audit serves GET /api/library/audit: stubs whose index disagrees with the engine's
// stored file list. Read-only, on demand.
func (h *Handler) Audit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	rep, err := h.mgr.Audit()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// readBody reads at most maxBodyBytes+1: reading exactly the cap accepts a valid JSON
// value followed by arbitrary excess, which mutates state on an effectively unsized
// request.
func readBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBodyBytes {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxBodyBytes)
	}
	return body, nil
}

func decode(r *http.Request, dst interface{}) error {
	body, err := readBody(r)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // headers are sent; a dead client is not actionable
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeAPIError(w http.ResponseWriter, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.Status, apiErr.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}
