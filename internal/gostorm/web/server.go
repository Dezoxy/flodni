package web

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"time"

	"flodni/internal/apiauth"
	"flodni/internal/gostorm/log"
	"flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/version"
	"flodni/internal/gostorm/web/api"
	"flodni/internal/gostorm/web/auth"
	"flodni/internal/gostorm/web/blocker"
	"flodni/internal/gostorm/web/sslcerts"
	"flodni/internal/redact"

	"github.com/gin-contrib/location/v2"
	"github.com/gin-gonic/gin"
	"github.com/wlynxg/anet"
)

var (
	BTS      = torr.NewBTS()
	waitChan = make(chan error, 3)

	// APIToken, when set before Start, is required as a bearer token on every
	// route except GET /echo. Empty: no auth. Auth (scope.go) supersedes it.
	APIToken string

	// AllowedHosts, when non-empty and set before Start, refuses a request whose
	// Host hostname is not listed (DNS rebinding defence), token or not.
	AllowedHosts []string

	// Logger receives the Origin refusals of the engine's gate. Nil logs nothing.
	Logger *slog.Logger
)

// newServer wraps the gin engine at the http.Server level so no route bypasses
// the token. Only the header read and idle connections are bounded: /stream
// and /torrent/upload are long-lived or large, so no read or write timeout.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           apiauth.GoStorm("", Logger, h, apiauth.WithTokens(authTokens()), apiauth.WithObserver(AuthObserver), apiauth.WithAllowedHosts(AllowedHosts)),
		ErrorLog:          redact.ErrorLog(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// newRoute builds the engine's router. It sets no CORS headers: the control
// panel reaches the engine through its same-origin proxy and every other caller
// is a script, so no page on another origin has a reason to read a response.
func newRoute() *gin.Engine {
	route := gin.New()
	// No response is produced before the scope middleware runs, in any mode: a
	// redirect or an Allow header would answer a path the middleware never classified,
	// and the pattern it reads (c.FullPath) must be the one that is served.
	route.RedirectTrailingSlash = false
	route.RedirectFixedPath = false
	route.HandleMethodNotAllowed = false
	route.UseRawPath = false
	route.UseEscapedPath = false
	route.RemoveExtraSlash = false
	route.Use(log.WebLogger(), blocker.Blocker(), log.Recovery())
	// Before every route registration: gin freezes each route's handler chain when
	// it is registered, so a route registered earlier would skip the middleware. The
	// mode is fixed here; a router built for an enforced mode refuses a request with
	// no identity, never reads it as open.
	if t := authTokens(); t == nil || t.Mode() != apiauth.ModeOpen { // nil: fail closed
		route.Use(scopeMiddleware())
	}
	route.Use(location.Default())
	auth.SetupAuth(route)

	route.GET("/echo", echo)

	api.SetupRoute(route)
	return route
}

func Start() {
	log.TLogln("Start GoStorm " + version.Version + " torrent " + version.GetTorrentVersion())
	ips := GetLocalIps()
	if len(ips) > 0 {
		log.TLogln("Local IPs:", ips)
	}
	err := BTS.Connect()
	if err != nil {
		log.TLogln("BTS.Connect() error!", err) // waitChan <- err
		os.Exit(1)                              // return
	}

	gin.SetMode(gin.ReleaseMode)

	route := newRoute()
	inproc.Store(&handlerBox{h: route}) // Flodni's own calls reach the engine through this

	// check if https enabled
	if settings.Ssl {
		// if no cert and key files set in db/settings, generate new self-signed cert and key files
		if settings.BTsets.SslCert == "" || settings.BTsets.SslKey == "" {
			settings.BTsets.SslCert, settings.BTsets.SslKey = sslcerts.MakeCertKeyFiles(ips)
			log.TLogln("Saving path to ssl cert and key in db", settings.BTsets.SslCert, settings.BTsets.SslKey)
			settings.SetBTSets(settings.BTsets)
		}
		// verify if cert and key files are valid
		err = sslcerts.VerifyCertKeyFiles(settings.BTsets.SslCert, settings.BTsets.SslKey, settings.SslPort)
		// if not valid, generate new self-signed cert and key files
		if err != nil {
			log.TLogln("Error checking certificate and private key files:", err)
			settings.BTsets.SslCert, settings.BTsets.SslKey = sslcerts.MakeCertKeyFiles(ips)
			log.TLogln("Saving path to ssl cert and key in db", settings.BTsets.SslCert, settings.BTsets.SslKey)
			settings.SetBTSets(settings.BTsets)
		}
		go func() {
			log.TLogln("Start https server at", settings.IP+":"+settings.SslPort)
			waitChan <- newServer(settings.IP+":"+settings.SslPort, route).ListenAndServeTLS(settings.BTsets.SslCert, settings.BTsets.SslKey)
		}()
	}

	switch authTokens().Mode() { // a nil store reads as open here, and is closed by the gate
	case apiauth.ModeScoped:
		log.TLogln("Scoped-token auth enabled (all routes except GET /echo)")
	case apiauth.ModeLegacyToken:
		log.TLogln("Bearer-token auth enabled (all routes except GET /echo)")
	}
	go func() {
		log.TLogln("Start http server at", settings.IP+":"+settings.Port)
		waitChan <- newServer(settings.IP+":"+settings.Port, route).ListenAndServe()
	}()
}

func echo(c *gin.Context) {
	c.String(200, "%v", version.Version)
}

func GetLocalIps() []string {
	ifaces, err := anet.Interfaces()
	if err != nil {
		log.TLogln("Error get local IPs")
		return nil
	}
	var list []string
	for _, i := range ifaces {
		addrs, _ := anet.InterfaceAddrsByInterface(&i)
		if i.Flags&net.FlagUp == net.FlagUp {
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() {
					list = append(list, ip.String())
				}
			}
		}
	}
	sort.Strings(list)
	return list
}
