package daemon

import (
	"net/http"

	"flodni/internal/apiauth"
)

// controlHandlers holds one handler per control route. Run fills it while it
// builds the services each handler needs; registerControlRoutes then puts every
// route on the mux in one place, which is also what the route-to-scope tests
// drive with stub handlers. A nil field panics at registration.
type controlHandlers struct {
	PlexWebhook, Webhook                                                 func(http.ResponseWriter, *http.Request)
	Metrics, Profiling, Prometheus, TTFF                                 func(http.ResponseWriter, *http.Request)
	Sessions, TorrentsHealth, Startup                                    func(http.ResponseWriter, *http.Request)
	ScrapeTrigger, ScrapeStatus                                          func(http.ResponseWriter, *http.Request)
	Health, Live, Panel                                                  func(http.ResponseWriter, *http.Request)
	WarmupEvict, Config, GostormSettings, Restart                        func(http.ResponseWriter, *http.Request)
	LibraryAdd, LibraryRemove, LibraryList, LibraryInspect, LibraryAudit func(http.ResponseWriter, *http.Request)
	LibraryFile, LibraryConfirm                                          func(http.ResponseWriter, *http.Request)
}

// registerControlRoutes is the only place routes reach the control mux. Every
// pattern here needs a row in the route table (internal/apiauth/scopes.go).
func registerControlRoutes(r *apiauth.Registrar, h controlHandlers) {
	r.Handle("/plex/webhook", h.PlexWebhook)
	r.Handle("/webhook", h.Webhook)
	r.Handle("/metrics", h.Metrics)
	r.Handle("/metrics/profiling", h.Profiling)
	r.Handle("/metrics/prometheus", h.Prometheus)
	r.Handle("/metrics/ttff", h.TTFF)
	r.Handle("/api/sessions", h.Sessions)
	r.Handle("/api/torrents/health", h.TorrentsHealth)
	r.Handle("/api/torrents/scrape", h.ScrapeTrigger)
	r.Handle("/api/torrents/scrape/status", h.ScrapeStatus)
	r.Handle("/api/health", h.Health)
	r.Handle("/api/live", h.Live)
	r.Handle("/api/startup", h.Startup)
	r.Handle("/api/warmup/evict", h.WarmupEvict)
	r.Handle("/control", h.Panel)
	r.Handle("/api/config", h.Config)
	r.Handle("/api/gostorm/settings", h.GostormSettings)
	r.Handle("/api/restart", h.Restart)
	r.Handle("/api/library/add", h.LibraryAdd)
	r.Handle("/api/library/remove", h.LibraryRemove)
	r.Handle("/api/library/list", h.LibraryList)
	r.Handle("/api/library/inspect", h.LibraryInspect)
	r.Handle("/api/library/audit", h.LibraryAudit)
	r.Handle("/api/library/file", h.LibraryFile)
	r.Handle("/api/library/confirm", h.LibraryConfirm)
}

// handleLive answers GET /api/live: the process serves. It checks nothing, so a
// container or load balancer can poll it cheaply; GET /api/health runs the
// mount, engine and database checks and reports warnings. GET only, like the
// route table and the gate's open set.
func handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}
