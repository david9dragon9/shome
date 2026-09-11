// Package ownerweb serves the machine owner's view of their own computer.
//
// Separate from the admin console, and served by the agent rather than the
// controller, because the owner's perspective is local. Every node has an
// owner; only one machine has a controller. A page that lived on the
// controller would be unreachable from the machine whose owner it is for.
//
// Loopback only, and unauthenticated on purpose: it exposes exactly what
// somebody sitting at this computer can already see with `shome monitor`, and
// asking the machine's owner for a cluster credential to look at their own
// hardware would be backwards. Binding it off loopback would change that, so
// it refuses to.
package ownerweb

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/agent"
	"github.com/davidwu/shome/internal/owner"
)

//go:embed web/monitor.html
var monitorHTML []byte

// Server is the owner's local view.
type Server struct {
	root string
	log  *slog.Logger
}

// Listen prepares the owner's page.
//
// Refuses a non-loopback address. The page has no authentication -- it is for
// whoever is at the keyboard -- so serving it to the network would publish
// this machine's state and hand its pause control to anyone on the LAN.
func Listen(root, addr string, log *slog.Logger) (*http.Server, net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("owner monitor address %q: expected host:port", addr)
	}
	if !isLoopback(host) {
		shown := host
		if shown == "" {
			shown = addr + " (no host part, which means every interface)"
		}
		return nil, nil, fmt.Errorf(
			"the owner monitor may only listen on loopback, not %q.\n"+
				"It has no login: it shows this machine's state and can pause it,\n"+
				"which is for whoever is sitting here. Use an ssh tunnel to reach\n"+
				"it from elsewhere", shown)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	s := &Server{root: root, log: log}
	m := http.NewServeMux()
	m.HandleFunc("/", s.page)
	m.HandleFunc("GET /api/status", s.status)
	m.HandleFunc("POST /api/pause", s.pause)
	m.HandleFunc("POST /api/resume", s.resume)
	return &http.Server{Handler: m, ReadHeaderTimeout: 5 * time.Second}, ln, nil
}

// isLoopback reports whether a host part names this machine and only this
// machine.
//
// An empty host is NOT loopback, which is the whole point of checking here.
// ":7821" splits into an empty host, and net.Listen takes that as every
// interface -- so reading it as "local" would bind the one page with no login
// to the LAN, through a check written to prevent exactly that. A hostname is
// refused too: what it resolves to is not this function's to decide, and a
// name that resolves off-box would pass on a technicality.
func isLoopback(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	w.Write(monitorHTML)
}

// status returns what the agent published, verbatim.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st, err := agent.ReadStatus(s.root)
	if err != nil {
		// A missing file is the ordinary case on a machine where shome has
		// never run, and deserves a usable answer rather than a 500.
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{
				"available": false,
				"reason": "shome has not published any status on this machine yet. " +
					"It may not be running here.",
			})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "status": st})
}

// pause and resume are the owner's veto, which needs no cluster involvement.
func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if reason == "" {
		reason = "paused from the monitor"
	}
	m := owner.NewManager(s.root, owner.DarwinSensor{})
	if err := m.Pause(reason); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("owner paused this machine", "reason", reason, "via", "monitor page")
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused", "reason": reason})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	m := owner.NewManager(s.root, owner.DarwinSensor{})
	if err := m.Resume(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("owner resumed this machine", "via", "monitor page")
	writeJSON(w, http.StatusOK, map[string]string{"status": "contributing"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
