package userenv

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Getting uv onto a machine that does not have it.
//
// uv is not the machine's software, it is shome's: every session's PATH
// names it, the base environment is built around it, and a session without
// it cannot install anything, which is the point of having one. On a Mac it
// arrives with the session image, and `shome admin env install` copies the
// admin's own copy for the native path. A Linux node gets neither -- and a
// freshly joined Debian machine has no uv and no apt package that provides
// one, so the cluster's environment simply is not there.
//
// So shome fetches it, from the project's own releases, and checks what it
// got against the checksum published beside it.
//
// The earlier refusal to do this -- "fetching and running a binary from the
// network on a machine someone has lent you is not a decision shome should
// make by itself" -- was the right instinct applied to the wrong thing. The
// machine already downloaded and ran shome itself to join, from a URL an
// admin pasted; uv arrives the same way, from a fixed upstream, verified.
// What that instinct protects is still protected: the base *system*
// packages are only ever installed with the machine's own package manager,
// and only when that needs no password.

// uvRelease is where uv's own builds live. Pinned to "latest" rather than a
// version: a cluster wants the uv its users expect, and uv is upgraded
// through this path only when a machine has none at all.
const uvRelease = "https://github.com/astral-sh/uv/releases/latest/download"

// uvDownloadTimeout bounds the fetch. Two binaries of about thirty
// megabytes; generous for a slow link, bounded so a node's startup cannot
// hang on a stalled connection.
const uvDownloadTimeout = 5 * time.Minute

// ensureUV puts uv and uvx in the managed tool directory.
//
// The machine's own copy is preferred when it has one: an admin who
// installed a particular uv meant it, and copying is faster and needs no
// network. Otherwise it is downloaded.
func ensureUV(root string) error {
	if err := os.MkdirAll(ToolDir(root), 0o755); err != nil {
		return err
	}
	if uvPresent(root) {
		return nil
	}
	if _, err := Install(root); err == nil {
		return nil
	}
	return downloadUV(root)
}

// uvPresent reports whether the managed directory already has a working uv.
func uvPresent(root string) bool {
	for _, name := range []string{"uv", "uvx"} {
		fi, err := os.Stat(filepath.Join(ToolDir(root), name))
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			return false
		}
	}
	return true
}

// uvTarget is the release asset for this machine, or "" if uv does not
// publish one for it.
func uvTarget() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "uv-x86_64-unknown-linux-gnu"
	case "linux/arm64":
		return "uv-aarch64-unknown-linux-gnu"
	case "darwin/amd64":
		return "uv-x86_64-apple-darwin"
	case "darwin/arm64":
		return "uv-aarch64-apple-darwin"
	}
	return ""
}

// downloadUV fetches uv for this machine and verifies it.
func downloadUV(root string) error {
	target := uvTarget()
	if target == "" {
		return fmt.Errorf("uv publishes no build for %s/%s; install it yourself "+
			"and re-run 'shome admin env install'", runtime.GOOS, runtime.GOARCH)
	}
	cl := &http.Client{Timeout: uvDownloadTimeout}
	asset := target + ".tar.gz"

	want, err := fetchText(cl, uvRelease+"/"+asset+".sha256")
	if err != nil {
		return fmt.Errorf("fetch uv's checksum: %w", err)
	}
	// The file is "<hex>  <name>", as sha256sum writes it.
	want = strings.TrimSpace(strings.Fields(want)[0])

	body, err := fetchBytes(cl, uvRelease+"/"+asset)
	if err != nil {
		return fmt.Errorf("download uv: %w", err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		// Refused rather than warned about: a binary that does not match
		// its published checksum is not uv, whatever it is.
		return fmt.Errorf("the uv download does not match its published checksum "+
			"(got %s, expected %s); nothing was installed", got[:16], want[:16])
	}
	n, err := extractUV(body, ToolDir(root))
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("the uv archive contained neither uv nor uvx")
	}
	return nil
}

// extractUV writes uv and uvx out of the release tarball, and nothing else.
//
// Named entries only, and the base name only: an archive is somebody else's
// file format, and a path inside one is not a path to trust. "../" in an
// entry name is how an extractor writes outside the directory it was given.
func extractUV(archive []byte, dir string) (int, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return 0, fmt.Errorf("uv archive is not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return n, fmt.Errorf("read the uv archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(h.Name)
		if base != "uv" && base != "uvx" {
			continue
		}
		dst := filepath.Join(dir, base)
		tmp := dst + ".partial"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return n, err
		}
		// Bounded: a decompression bomb must not fill the disk of a machine
		// somebody lent us. uv is about 35 MB.
		if _, err := io.Copy(f, io.LimitReader(tr, 256<<20)); err != nil {
			f.Close()
			os.Remove(tmp)
			return n, err
		}
		f.Close()
		// Renamed into place, so a half-written binary is never on PATH.
		if err := os.Rename(tmp, dst); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func fetchBytes(cl *http.Client, url string) ([]byte, error) {
	resp, err := cl.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}

func fetchText(cl *http.Client, url string) (string, error) {
	b, err := fetchBytes(cl, url)
	return string(b), err
}
