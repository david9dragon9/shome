package ctl

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/userfs"
)

// Storage manages per-user persistent files on the controller machine.
//
// It is a view onto the same per-machine, per-user area that `shome fs` uses
// -- the controller is just another machine -- so a file put here is the file
// `shome fs ls <controller>:` lists, counted once against one cluster-wide
// limit. It used to be a second directory with its own quota check, which
// meant an account could hold its full allowance twice by using both
// commands.
//
// Quotas are accounted by walking the user's tree rather than by maintaining a
// counter. A counter drifts: every failed upload, partial write or manual
// deletion is a chance to lose sync, and a storage quota that quietly diverges
// from reality is worse than none. Walking is exact, and a home cluster's file
// counts are small enough that a short cache makes it cheap.
type Storage struct {
	files *userfs.Store

	mu    sync.Mutex
	cache map[string]usageEntry

	// onChange, when set, is told that an account's files here have changed,
	// so this machine's own agent re-measures rather than waiting for its
	// next scheduled scan.
	//
	// A hook rather than a direct dependency, for the reason the revocation
	// hook is one: the controller must work on a machine that runs no agent.
	// Without it, `shome storage put` was followed by `shome fs ls` saying
	// "you have no files in the cluster yet" for up to twenty seconds --
	// which reads as the upload having been lost.
	onChange func(user string)
}

type usageEntry struct {
	bytes int64
	at    time.Time
}

// usageTTL is how long a computed usage figure is reused. Short enough that
// quota decisions are not made on stale data.
const usageTTL = 2 * time.Second

// NewStorage roots per-user files where every machine keeps them, so the
// controller's own agent indexes and reports them like any other node's.
func NewStorage(nodeRoot string) *Storage {
	return &Storage{files: userfs.New(nodeRoot), cache: map[string]usageEntry{}}
}

// UserDir is a user's persistent area on this machine.
func (s *Storage) UserDir(user string) string { return s.files.UserDir(user) }

// Resolve maps a user-supplied relative path to an absolute path inside the
// user's own directory, refusing anything that escapes.
//
// Delegated rather than reimplemented: this is the boundary that makes
// per-user isolation real, and two separately-written versions of one
// security check is how they drift apart.
func (s *Storage) Resolve(user, rel string) (string, error) {
	return s.files.Resolve(user, rel)
}

// Usage returns the bytes a user occupies: their persistent files plus the job
// artefacts shome keeps on their behalf.
func (s *Storage) Usage(user string) (int64, error) {
	s.mu.Lock()
	if e, ok := s.cache[user]; ok && time.Since(e.at) < usageTTL {
		s.mu.Unlock()
		return e.bytes, nil
	}
	s.mu.Unlock()

	total, err := dirSize(s.UserDir(user))
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	s.cache[user] = usageEntry{bytes: total, at: time.Now()}
	s.mu.Unlock()
	return total, nil
}

// SetChangeHook registers what to tell when an account's files change here.
func (s *Storage) SetChangeHook(f func(user string)) {
	s.mu.Lock()
	s.onChange = f
	s.mu.Unlock()
}

// Invalidate drops a cached usage figure after a write, and tells whoever
// else is holding a measurement of this account.
func (s *Storage) Invalidate(user string) {
	s.mu.Lock()
	delete(s.cache, user)
	f := s.onChange
	s.mu.Unlock()
	if f != nil {
		f(user)
	}
}

// mib formats a byte count without rounding sub-megabyte values to zero --
// "0 MiB used of 1 MiB" is a confusing thing to tell someone over quota.
func mib(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(b)/1024)
	default:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	}
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // nothing stored yet
			}
			return err
		}
		if fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return total, err
}

// Entry describes one file or directory in a listing.
type Entry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
	MTime string `json:"mtime"`
}

func (s *Storage) List(user, rel string) ([]Entry, error) {
	full, err := s.Resolve(user, rel)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return []Entry{}, nil
		}
		return nil, err
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		fi, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{
			Name: de.Name(), Size: fi.Size(), IsDir: de.IsDir(),
			MTime: fi.ModTime().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Put writes a file, bounded by how many more bytes the account may store.
//
// The budget is what is left of the account's *cluster-wide* allowance, not
// this directory's share of it, because this is one of several machines
// holding the same allowance. It is checked against the amount actually
// written as the copy proceeds rather than up front: a client can lie about
// Content-Length, and a stream has no declared size at all.
//
// A negative budget means unlimited; zero means nothing more may be stored.
// That matches how QoS limits resolve, so the two do not need translating
// between -- an earlier version had zero meaning unlimited here and
// none-allowed there, which turned "this account may store nothing" into
// "this account may store anything".
func (s *Storage) Put(user, rel string, r io.Reader, budget int64) error {
	full, err := s.Resolve(user, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	if budget == 0 {
		return fmt.Errorf("you have no storage allowance left.\n" +
			"See where it has gone with 'shome fs du'")
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	var written int64
	if budget < 0 {
		written, err = io.Copy(tmp, r)
	} else {
		// Read one byte past the budget so exceeding it is detectable.
		written, err = io.Copy(tmp, io.LimitReader(r, budget+1))
	}
	if err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if budget >= 0 && written > budget {
		return fmt.Errorf("this file would put you over your cluster-wide storage limit.\n"+
			"You have %s left; see where the rest has gone with 'shome fs du'", mib(budget))
	}
	if err := os.Rename(tmp.Name(), full); err != nil {
		return err
	}
	s.Invalidate(user)
	return nil
}

func (s *Storage) Open(user, rel string) (io.ReadCloser, error) {
	full, err := s.Resolve(user, rel)
	if err != nil {
		return nil, err
	}
	return os.Open(full)
}

func (s *Storage) Remove(user, rel string) error {
	if rel == "" || rel == "." {
		return fmt.Errorf("refusing to remove your entire storage area; name a path")
	}
	full, err := s.Resolve(user, rel)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(full); err != nil {
		return err
	}
	s.Invalidate(user)
	return nil
}
