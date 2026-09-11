package ctl

import (
	_ "embed"
	"net"
	"net/http"
	"time"
)

//go:embed web/console.html
var consoleHTML []byte

// ServeWeb starts the admin web console.
//
// Same handlers as the CLI uses, under /api/v1, so the console can never do
// something the API does not also expose -- anything clickable is scriptable,
// and there is no privileged back door for the UI.
//
// Binds to loopback unless told otherwise: a home cluster's controller often
// sits on a laptop, and a console reachable from the whole LAN by default
// would be a poor surprise.
func (a *API) WebHandler() http.Handler {
	m := http.NewServeMux()
	// Registered without a method so it does not conflict with the /api/v1/
	// subtree; Go 1.22+ routing rejects a method-specific "GET /" alongside a
	// more specific path pattern that accepts all methods.
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The console holds a bearer token in localStorage, so keep it from
		// being framed or sniffed into another content type.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
		w.Write(consoleHTML)
	})
	// The whole JSON API, authenticated exactly as the CLI is.
	m.Handle("/api/v1/", http.StripPrefix("/api/v1", a.routes()))
	return m
}

// ServeConsole starts the web console listener.
func ServeConsole(c *Controller, addr string) (*http.Server, net.Listener, error) {
	api := &API{c: c}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler:           api.WebHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv, ln, nil
}
