package ctl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/store"
)

type ctxKey int

const userKey ctxKey = 0

// callerFrom returns the authenticated user attached by requireAuth.
func callerFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userKey).(*store.User)
	return u
}

// requireAuth authenticates every client request by bearer token.
//
// The socket is already owner-only, but that authenticates the *machine
// account*, not the shome user. Once several people share a cluster, identity
// has to come from something the caller presents -- otherwise "who submitted
// this" is unanswerable and any local process could act as anyone.
func (a *API) requireAuth(next func(http.ResponseWriter, *http.Request, *store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			fail(w, http.StatusUnauthorized,
				"no API token; set SHOME_TOKEN or run as the cluster owner")
			return
		}
		u, err := a.c.Store().UserByToken(r.Context(), tok)
		if err != nil && !errors.Is(err, store.ErrAccountDisabled) {
			// A login session holds a short-lived credential scoped to its
			// account rather than the account's own token, which is stored
			// only as a hash and cannot be handed out. Tried second so the
			// ordinary path costs one lookup.
			//
			// Not tried when the first lookup found a suspended account: that
			// is an answer, and falling through would replace it with the
			// less useful "invalid token".
			u, err = a.c.Store().UserBySessionToken(r.Context(), tok, time.Now())
		}
		switch {
		case errors.Is(err, store.ErrAccountDisabled):
			// Distinguished from an unrecognised token, on purpose.
			//
			// The uniform answer would be defensible if it withheld anything,
			// and it does not: reaching this line means presenting a genuine
			// credential, so whoever holds it already knows the account
			// exists. What the merged answer cost was the person who had just
			// been quarantined, and the admin looking into why their jobs
			// stopped, both being told their token was invalid -- which sent
			// them after a credential that was never the problem.
			//
			// A token is 32 random bytes, so this is not an oracle worth
			// having: confirming a guess requires making one.
			fail(w, http.StatusForbidden,
				"this account is suspended; ask an administrator to lift it")
			return
		case err != nil:
			// Everything else is one answer deliberately: an unknown token, a
			// malformed one and an expired session credential are all just
			// "this is not a credential", and telling them apart would say
			// which guesses were closer.
			fail(w, http.StatusUnauthorized, "invalid API token")
			return
		}
		// Impersonation, admin-only. The login shell runs on the controller
		// host as its owner and must act on behalf of whichever principal the
		// SSH certificate authenticated. Restricting it to admin tokens keeps
		// it from becoming a general privilege-escalation path, and every use
		// is recorded.
		if as := strings.TrimSpace(r.Header.Get("X-Shome-Act-As")); as != "" {
			if u.Role != store.RoleAdmin {
				fail(w, http.StatusForbidden, "only an admin may act as another user")
				return
			}
			// A suspended account must be suspended everywhere. Checking only
			// at login would let an SSH session opened beforehand keep working
			// after a quarantine, which is precisely when it matters -- and
			// the lookup refuses one, so this is where that is enforced.
			//
			// Separated from a name that resolves to nothing, because they are
			// different mistakes: a suspended account is a decision somebody
			// made and can lift, a missing one is a typo or a deleted account.
			target, err := a.c.Store().UserByName(r.Context(), as)
			switch {
			case errors.Is(err, store.ErrAccountDisabled):
				fail(w, http.StatusForbidden, "cannot act as "+as+": that account is suspended")
				return
			case err != nil:
				fail(w, http.StatusBadRequest, "cannot act as "+as+": "+err.Error())
				return
			}
			a.c.log.Info("acting as user", "admin", u.Name, "as", target.Name, "path", r.URL.Path)
			u = target
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)), u)
	}
}

// requireRole is requireAuth plus a rank check: the caller's role must be at
// least min.
//
// Every route goes through this, including the ones open to any account --
// those ask for store.RoleUser, the lowest rank, so "any authenticated
// account" and "an operator or better" are the same mechanism with a
// different argument rather than two code paths that could diverge.
//
// Roles are ranked rather than enumerated so that a route added later cannot
// accidentally exclude admins from something operators may do.
func (a *API) requireRole(min store.Role, next func(http.ResponseWriter, *http.Request, *store.User)) http.HandlerFunc {
	return a.requireAuth(func(w http.ResponseWriter, r *http.Request, u *store.User) {
		if !u.Role.AtLeast(min) {
			fail(w, http.StatusForbidden,
				fmt.Sprintf("this action requires the %s role or higher (you are %s)", min, u.Role))
			return
		}
		next(w, r, u)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// canTouchJob reports whether u may act on a job owned by owner.
func canTouchJob(u *store.User, owner string) bool {
	return u.Role == store.RoleAdmin || u.Name == owner
}

// AdminTokenPath is where the bootstrap admin token is written, so the owner's
// CLI works on a fresh install without a setup step.
func AdminTokenPath(root string) string { return filepath.Join(root, "admin.token") }

// EnsureAdminToken guarantees the machine running the controller has a working
// owner credential on disk.
//
// It creates the first admin account on an empty cluster, and -- importantly --
// reissues the token if the file has gone missing from a cluster that already
// has accounts. Tokens are stored hashed, so a deleted admin.token used to be
// unrecoverable: the owner was locked out of their own cluster by the loss of
// one file, with no path back short of editing the database.
func EnsureAdminToken(ctx context.Context, st *store.Store, root string) (wrote bool, err error) {
	if created, err := BootstrapAdmin(ctx, st, root); err != nil || created {
		return created, err
	}
	if _, err := os.Stat(AdminTokenPath(root)); err == nil {
		return false, nil
	}
	// An existing cluster whose owner has no token file. Reissue for the first
	// admin account, preferring one named after the current user.
	users, err := st.Users(ctx)
	if err != nil {
		return false, err
	}
	target := ""
	for _, u := range users {
		if u.Role != store.RoleAdmin {
			continue
		}
		if u.Name == os.Getenv("USER") {
			target = u.Name
			break
		}
		if target == "" {
			target = u.Name
		}
	}
	if target == "" {
		return false, nil // no admin account to reissue for; nothing safe to do
	}
	tok, err := st.ResetUserToken(ctx, target)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(AdminTokenPath(root), []byte(tok+"\n"), 0o600); err != nil {
		return false, fmt.Errorf("write admin token: %w", err)
	}
	return true, nil
}

// BootstrapAdmin creates the first admin account on an empty cluster.
func BootstrapAdmin(ctx context.Context, st *store.Store, root string) (created bool, err error) {
	n, err := st.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	name := os.Getenv("USER")
	if name == "" {
		name = "admin"
	}
	if err := validName(name); err != nil {
		name = "admin"
	}
	tok, err := st.CreateUser(ctx, name, store.RoleAdmin, 0, time.Now())
	if err != nil {
		return false, err
	}
	// 0600: this file is the cluster's root credential.
	if err := os.WriteFile(AdminTokenPath(root), []byte(tok+"\n"), 0o600); err != nil {
		return false, fmt.Errorf("write admin token: %w", err)
	}
	return true, nil
}

func validName(name string) error {
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("bad name")
		}
	}
	if name == "" || len(name) > 32 {
		return fmt.Errorf("bad name")
	}
	return nil
}
