package ctl

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/store"
)

// The bootstrap service exists to collapse joining a machine from a
// multi-step, error-prone ritual -- mint a token, work out the controller's
// LAN address, cross-compile for the target, copy the binary across, then run
// an agent with four flags whose values must agree -- into one line the user
// pastes on the new machine.
//
// It is plain HTTP, deliberately, because the client is `curl` on a machine
// that has nothing installed yet. What makes that acceptable:
//
//   - Every path is scoped by a join token that is single-use, time-boxed, and
//     unguessable, so this is not an open binary server.
//   - It hands out only the shome binary and a script that runs it.
//   - The join it leads to is mutual TLS; the token buys a certificate, and
//     nothing the bootstrap service says is trusted after that point.
//
// A tampered binary on a hostile network would still be a real attack. The
// threat model here is a home LAN with trusted users, and the alternative --
// telling people to copy binaries around by hand -- is what this replaces.

// BootstrapPathPrefix scopes every bootstrap route under a token.
const BootstrapPathPrefix = "/j/"

// ServeBootstrap returns an unstarted HTTP server that hands new machines the
// shome binary and the exact command to join with.
func ServeBootstrap(st *store.Store, root, addr string, log *slog.Logger) (*http.Server, net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	b := &bootstrap{st: st, root: root, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /j/{token}/install", b.install)
	mux.HandleFunc("GET /j/{token}/bin/{platform}", b.binary)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Give a human who opens this port in a browser something honest.
		http.Error(w, "shome join service: needs an invitation.\n"+
			"Run 'shome invite' on the controller to get one.\n", http.StatusNotFound)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv, ln, nil
}

type bootstrap struct {
	st   *store.Store
	root string
	log  *slog.Logger
}

// authorize checks the token in the path. It does not consume it: the token
// has to survive the download so the join itself can redeem it.
func (b *bootstrap) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	tok := r.PathValue("token")
	if tok == "" || !b.st.JoinTokenValid(r.Context(), tok, time.Now()) {
		http.Error(w, "this invitation is not valid, has expired, or has already "+
			"been used.\nRun 'shome invite' on the controller for a new one.\n",
			http.StatusForbidden)
		return "", false
	}
	return tok, true
}

// controllerAddr is the address to tell the new machine to dial.
//
// It comes from the Host header rather than from anything the controller
// guesses about itself, because the client has just demonstrated that this
// address reaches us. Guessing is how a machine ends up told to connect to a
// LAN address that is correct for the controller and unreachable from there.
func (b *bootstrap) controllerAddr(r *http.Request) string {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		return r.Host
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(p-1))
}

func (b *bootstrap) install(w http.ResponseWriter, r *http.Request) {
	tok, ok := b.authorize(w, r)
	if !ok {
		return
	}
	base := "http://" + r.Host + BootstrapPathPrefix + tok
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	fmt.Fprint(w, installScript(base, b.controllerAddr(r)))
}

func (b *bootstrap) binary(w http.ResponseWriter, r *http.Request) {
	if _, ok := b.authorize(w, r); !ok {
		return
	}
	platform := r.PathValue("platform")
	path, err := b.binaryPath(platform)
	if err != nil {
		b.log.Warn("bootstrap: no binary to serve", "platform", platform, "err", err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "cannot read the binary for "+platform, http.StatusInternalServerError)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot read the binary for "+platform, http.StatusInternalServerError)
		return
	}
	b.log.Info("bootstrap: serving binary", "platform", platform,
		"remote", r.RemoteAddr, "bytes", fi.Size())
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	io.Copy(w, f)
}

// binaryPath finds a shome binary for the requested platform.
//
// Prebuilt binaries under <root>/dist win; failing that, if the caller wants
// the platform this controller is running on, our own executable is by
// definition the right one. That second case is what makes joining a second
// Mac work with no cross-compilation, which is impossible anyway: the macOS
// backend needs cgo for Metal and libproc.
func (b *bootstrap) binaryPath(platform string) (string, error) {
	if strings.ContainsAny(platform, "/\\.") || platform == "" {
		return "", fmt.Errorf("bad platform name")
	}
	// The state root first, then the per-user location install.sh writes to.
	// Two places because the state root is shared and may be read-only to the
	// person who ran the installer, while ~/.shome always belongs to them.
	var dirs []string
	dirs = append(dirs, filepath.Join(b.root, "dist"))
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".shome", "dist"))
	}
	for _, d := range dirs {
		p := filepath.Join(d, platform, "shome")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	if platform == runtime.GOOS+"-"+runtime.GOARCH {
		if self, err := os.Executable(); err == nil {
			if self, err = filepath.EvalSymlinks(self); err == nil {
				return self, nil
			}
		}
	}
	return "", fmt.Errorf("this controller has no shome binary for %s.\n"+
		"Build one there instead:  git clone <repo> && cd shome && ./install.sh", platform)
}

// installScript is what the new machine pipes into sh. It is written to be
// readable, because anyone sensible reads a script before piping it to a
// shell, and to fail with an explanation rather than a stack of shell errors.
func installScript(base, controller string) string {
	return `#!/bin/sh
# shome join script. Downloads the shome binary for this machine and connects
# it to the cluster. Everything it needs is baked into this URL.
set -e

controller='` + controller + `'
base='` + base + `'
name="${SHOME_NAME:-$(uname -n | cut -d. -f1)}"
bindir="${SHOME_BIN:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "shome: unsupported CPU architecture '$arch'" >&2; exit 1 ;;
esac

echo "shome: installing for $os-$arch as node '$name'"

mkdir -p "$bindir"
tmp="$bindir/.shome.download.$$"
trap 'rm -f "$tmp"' EXIT INT TERM

if ! curl -fsSL "$base/bin/$os-$arch" -o "$tmp"; then
  echo "shome: could not download a binary for $os-$arch." >&2
  echo "       The controller may not have one built for this platform." >&2
  exit 1
fi
# A truncated download produces a binary that fails in confusing ways later.
if [ ! -s "$tmp" ]; then
  echo "shome: the downloaded binary is empty." >&2
  exit 1
fi
chmod +x "$tmp"
mv "$tmp" "$bindir/shome"
trap - EXIT INT TERM
echo "shome: installed $bindir/shome"

"$bindir/shome" join "$controller" --name "$name" --token '` + tokenFromBase(base) + `'

case ":$PATH:" in
  *":$bindir:"*) ;;
  *)
    echo
    echo "Add shome to your PATH to use it from any shell:"
    echo "    echo 'export PATH=\"$bindir:\$PATH\"' >> ~/.profile"
    echo "    export PATH=\"$bindir:\$PATH\""
    ;;
esac
`
}

// tokenFromBase pulls the token back out of the base URL the handler built.
func tokenFromBase(base string) string {
	i := strings.LastIndex(base, BootstrapPathPrefix)
	if i < 0 {
		return ""
	}
	return base[i+len(BootstrapPathPrefix):]
}
