// Package userfs is per-user storage on one machine.
//
// Every node has its own copy, so a user's files exist per (user, machine)
// pair rather than in one place. That is the honest model for a cluster with
// no shared filesystem: there is no mount making a file on one machine
// visible from another, so a single namespace that silently meant "wherever
// the controller is" would make every path ambiguous the moment a second
// machine joined.
//
// The consequence users must understand is that copying a file to a second
// machine costs its size again. Slurm hides that behind a shared home; shome
// cannot, so it surfaces it: a per-machine breakdown, and a cluster-wide
// total the copy counts against.
package userfs

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Store is one machine's user storage.
type Store struct {
	root string // <node-root>/users
}

func New(nodeRoot string) *Store { return &Store{root: filepath.Join(nodeRoot, "users")} }

// Root is where this machine keeps user files.
func (s *Store) Root() string { return s.root }

// UserDir is one user's directory on this machine.
func (s *Store) UserDir(user string) string { return filepath.Join(s.root, user) }

// Resolve turns a user-supplied path into an absolute one, refusing anything
// that leaves the user's own directory.
//
// A leading slash is interpreted as relative to the user's own storage rather
// than rejected: no other filesystem is visible to them, so "/data" can only
// mean one thing, and rerooting it is safe because the prefix check below is
// applied to the result either way.
//
// The check is on the cleaned, joined result rather than on the input. A
// blocklist of ".." or of leading slashes is the kind of thing that looks
// right and is defeated by an encoding nobody thought of; comparing the final
// path against the prefix it must have cannot be talked around.
func (s *Store) Resolve(user, rel string) (string, error) {
	if err := ValidUser(user); err != nil {
		return "", err
	}
	base := s.UserDir(user)
	full := filepath.Clean(filepath.Join(base, rel))
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("%q is outside your storage", rel)
	}
	return full, nil
}

// ValidUser rejects names that cannot be a path component.
//
// A user's files live in a directory named after them, so a name containing a
// separator would place one person's storage inside another's.
func ValidUser(user string) error {
	if user == "" {
		return fmt.Errorf("no user given")
	}
	if user == "." || user == ".." || strings.ContainsAny(user, `/\`+"\x00") {
		return fmt.Errorf("invalid user name %q", user)
	}
	return nil
}

// Entry is one file or directory.
type Entry struct {
	Path  string    `json:"path"` // relative to the user's directory
	Size  int64     `json:"size"`
	Dir   bool      `json:"dir"`
	MTime time.Time `json:"mtime"`
}

// Usage is what one user occupies on this machine.
type Usage struct {
	Bytes  int64 `json:"bytes"`
	Files  int   `json:"files"`
	Inodes int   `json:"inodes"`
}

// List returns the entries directly under rel, like ls rather than find.
//
// Shallow on purpose: a recursive default would walk a large tree on every
// listing, and a caller wanting everything can ask for it.
func (s *Store) List(user, rel string) ([]Entry, error) {
	full, err := s.Resolve(user, rel)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			// A user who has stored nothing has no directory, which is an
			// empty listing rather than an error.
			if rel == "" || rel == "." {
				return nil, nil
			}
			return nil, fmt.Errorf("%s: no such file or directory", rel)
		}
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			continue
		}
		e := Entry{Path: filepath.Join(rel, de.Name()), Dir: de.IsDir(), MTime: info.ModTime()}
		if !de.IsDir() {
			e.Size = info.Size()
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Walk returns every file under a user's directory, for the index the
// controller keeps.
//
// Capped, and the cap is reported rather than silently applied: a user with a
// hundred thousand files should learn the listing is partial, not quietly get
// a wrong answer. The usage totals are exact regardless, because they are
// what limits are enforced against.
func (s *Store) Walk(user string, max int) (entries []Entry, usage Usage, truncated bool, err error) {
	base := s.UserDir(user)
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // a file that vanished mid-walk is normal
		}
		usage.Inodes++
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		usage.Files++
		usage.Bytes += info.Size()
		if max > 0 && len(entries) >= max {
			truncated = true
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return nil
		}
		entries = append(entries, Entry{Path: rel, Size: info.Size(), MTime: info.ModTime()})
		return nil
	})
	if err != nil && os.IsNotExist(err) {
		return nil, Usage{}, false, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, usage, truncated, err
}

// Users lists the accounts with storage on this machine.
func (s *Store) Users() ([]string, error) {
	des, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, de := range des {
		if de.IsDir() {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// UsageFor is one user's total on this machine.
func (s *Store) UsageFor(user string) (Usage, error) {
	_, u, _, err := s.Walk(user, 0)
	return u, err
}

// Put writes a file, up to limit bytes.
//
// The limit is checked as the copy proceeds rather than from a declared size:
// a caller can misreport a length, and a stream has none at all. A negative
// limit means unlimited; zero means nothing may be written.
func (s *Store) Put(user, rel string, r io.Reader, limit int64) (int64, error) {
	full, err := s.Resolve(user, rel)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".shome-upload-*")
	if err != nil {
		return 0, err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name()) // a no-op once renamed
	}()

	var written int64
	if limit < 0 {
		written, err = io.Copy(tmp, r)
	} else {
		// One byte past the limit, so exceeding it is detected rather than
		// silently truncating the file to fit.
		written, err = io.Copy(tmp, io.LimitReader(r, limit+1))
		if err == nil && written > limit {
			return 0, fmt.Errorf("this would exceed your storage limit by at least %d byte(s)",
				written-limit)
		}
	}
	if err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	// Rename last, so a reader never sees a half-written file and a failed
	// transfer leaves the previous version intact.
	if err := os.Rename(tmp.Name(), full); err != nil {
		return 0, err
	}
	return written, nil
}

// Open reads a file.
func (s *Store) Open(user, rel string) (io.ReadCloser, int64, error) {
	full, err := s.Resolve(user, rel)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("%s: no such file", rel)
		}
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	if fi.IsDir() {
		f.Close()
		return nil, 0, fmt.Errorf("%s is a directory", rel)
	}
	return f, fi.Size(), nil
}

// Remove deletes a file, or a directory tree when recursive.
func (s *Store) Remove(user, rel string, recursive bool) error {
	full, err := s.Resolve(user, rel)
	if err != nil {
		return err
	}
	// "rm with no argument deletes everything" is not a behaviour anyone
	// should discover.
	if full == s.UserDir(user) {
		return fmt.Errorf("refusing to remove your entire storage; name a path inside it")
	}
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: no such file or directory", rel)
		}
		return err
	}
	if fi.IsDir() && !recursive {
		return fmt.Errorf("%s is a directory; pass -r to remove it and its contents", rel)
	}
	if fi.IsDir() {
		return os.RemoveAll(full)
	}
	return os.Remove(full)
}

// Mkdir creates a directory.
func (s *Store) Mkdir(user, rel string) error {
	full, err := s.Resolve(user, rel)
	if err != nil {
		return err
	}
	return os.MkdirAll(full, 0o700)
}
