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

	"github.com/gin-contrib/location/v2"
	"github.com/gin-gonic/gin"
	"github.com/wlynxg/anet"
)

var (
	BTS      = torr.NewBTS()
	waitChan = make(chan error, 3)

	// APIToken, when set before Start, is required as a bearer token on every
	// route except GET /echo. Empty: no auth.
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
		Handler:           apiauth.GoStorm(APIToken, Logger, h, apiauth.WithAllowedHosts(AllowedHosts)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// newRoute builds the engine's router. It sets no CORS headers: the control
// panel reaches the engine through its same-origin proxy and every other caller
// is a script, so no page on another origin has a reason to read a response.
func newRoute() *gin.Engine {
	route := gin.New()
	route.Use(log.WebLogger(), blocker.Blocker(), gin.Recovery(), location.Default())
	auth.SetupAuth(route)
	if APIToken != "" {
		// Requests reaching a handler carried the token (apiauth.GoStorm gates them).
		route.Use(func(c *gin.Context) { c.Set(api.TokenEnforcedKey, true) })
	}

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

	if APIToken != "" {
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
