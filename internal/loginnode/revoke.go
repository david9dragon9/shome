package loginnode

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/store"
)

// Revoking access from a session that is already open.
//
// Checking a key at login is not enough. A person signed in on a borrowed
// computer, or a laptop that has just been reported lost, holds a live
// connection that authentication will not be consulted about again -- so
// without this, "signed out" would mean "cannot sign in again", and the
// session someone was worried about would carry on working.
//
// Two mechanisms, because they fail in different ways:
//
//   - A direct hook, so revoking through shome closes the session at once.
//     This is the fast path and covers every route into Unenroll.
//   - A recheck before each command and on a timer, so a session also dies
//     when the hook could not have fired: another process holding the same
//     database, an admin editing it directly, or a restart that lost the
//     registry. Slower, but it does not depend on anything having been wired
//     up correctly.

// recheckInterval is how often an idle session revalidates itself. Short
// enough that a revoked session goes away while somebody is still watching for
// it to, long enough to be a negligible query load.
const recheckInterval = 5 * time.Second

// live tracks open sessions so they can be closed from outside.
type live struct {
	mu       sync.Mutex
	sessions map[int64]*liveSession
	next     int64
}

type liveSession struct {
	user   string
	keyFP  string
	cancel context.CancelFunc
	notify func(string)
}

func newLive() *live { return &live{sessions: map[int64]*liveSession{}} }

func (l *live) add(s *liveSession) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	l.sessions[l.next] = s
	return l.next
}

func (l *live) remove(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.sessions, id)
}

// closeFor ends every session belonging to user, optionally narrowed to one
// key. An empty fingerprint means all of their machines.
func (l *live) closeFor(user, fingerprint, reason string) int {
	l.mu.Lock()
	var doomed []*liveSession
	for _, s := range l.sessions {
		if s.user != user {
			continue
		}
		if fingerprint != "" && s.keyFP != fingerprint {
			continue
		}
		doomed = append(doomed, s)
	}
	l.mu.Unlock()

	// Outside the lock: notifying writes to a network connection, and holding
	// a mutex across that would let one wedged client block every revocation.
	for _, s := range doomed {
		if s.notify != nil {
			s.notify(reason)
		}
		s.cancel()
	}
	return len(doomed)
}

// CloseSessionsFor ends open sessions whose access has just been withdrawn.
//
// Wired to the controller so that unenrolling takes effect immediately rather
// than at the next login attempt.
func (s *Server) CloseSessionsFor(user, fingerprint string) int {
	if s == nil || s.live == nil {
		return 0
	}
	n := s.live.closeFor(user, fingerprint,
		"\nYour access to this cluster has been withdrawn. Disconnecting.\n")
	if n > 0 {
		s.cfg.Log.Info("closed sessions after revocation",
			"user", user, "fingerprint", fingerprint, "sessions", n)
	}
	return n
}

// stillAuthorized reports whether a session's credentials remain valid.
//
// Consulted before every command and on a timer. Both matter: the pre-command
// check means a revoked user cannot do anything else even if the timer has not
// fired, and the timer means an idle session does not sit open indefinitely
// after its access is gone.
func (s *Server) stillAuthorized(ctx context.Context, user, keyFP string) error {
	// A suspended account and a deleted one both end the session, but the
	// message is what the person at the prompt is told, and "no longer
	// exists" for an account that has merely been quarantined sends them to
	// the wrong conversation with their admin.
	if _, err := s.cfg.Store.UserByName(ctx, user); err != nil {
		if errors.Is(err, store.ErrAccountDisabled) {
			return fmt.Errorf("your account has been suspended")
		}
		return fmt.Errorf("your account no longer exists")
	}
	// A certificate session has no registered key, so there is nothing to
	// check beyond the account; its own expiry bounds it instead.
	if keyFP == "" {
		return nil
	}
	if _, err := s.cfg.Store.UserKeyByFingerprint(ctx, user, keyFP); err != nil {
		return fmt.Errorf("this computer has been signed out")
	}
	return nil
}

// watchAuthorization cancels a session once its access is withdrawn.
func (s *Server) watchAuthorization(ctx context.Context, cancel context.CancelFunc,
	user, keyFP string, notify func(string)) {

	t := time.NewTicker(recheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check, done := context.WithTimeout(ctx, 5*time.Second)
			err := s.stillAuthorized(check, user, keyFP)
			done()
			if err != nil {
				if notify != nil {
					notify("\n" + err.Error() + ". Disconnecting.\n")
				}
				s.cfg.Log.Info("session revoked", "user", user, "reason", err)
				cancel()
				return
			}
		}
	}
}
