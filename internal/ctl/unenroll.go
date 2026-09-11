package ctl

import (
	"context"
	"fmt"
)

// Signing a machine out, from either side.
//
// One implementation for both the user doing it themselves and an admin doing
// it for them, because the two must not diverge. They did at first: an admin
// removing a key left the account's enrollment codes alive, while a user
// signing out spent them -- so the same words meant different things depending
// on who said them, and the weaker one was the path used to revoke a lost
// laptop.

// UnenrollResult reports what a sign-out removed.
type UnenrollResult struct {
	User             string   `json:"user"`
	Removed          []string `json:"removed"`
	CodesInvalidated int      `json:"codes_invalidated"`
}

// Unenroll removes one of a user's keys, or all of them, and always spends
// their outstanding enrollment codes.
//
// The codes go every time, deliberately. A key revoked because a machine was
// lost or borrowed, with a live code left behind, is not revoked -- whoever
// has the machine enrolls again. Somebody who still has another machine can
// issue themselves a fresh code in seconds, so the cost of being strict here
// is small and falls on the right person.
func (c *Controller) Unenroll(ctx context.Context, user, fingerprint string, all bool) (*UnenrollResult, error) {
	now := c.now()
	res := &UnenrollResult{User: user}

	switch {
	case all:
		keys, err := c.store.UserKeys(ctx, user)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			res.Removed = append(res.Removed, k.Fingerprint)
		}
		if err := c.store.DeleteUserKeys(ctx, user); err != nil {
			return nil, err
		}
	case fingerprint != "":
		// Ownership first. Deleting by fingerprint alone would let one
		// account's sign-out revoke another's machine.
		if _, err := c.store.UserKeyByFingerprint(ctx, user, fingerprint); err != nil {
			return nil, err
		}
		if err := c.store.DeleteUserKey(ctx, fingerprint, now); err != nil {
			return nil, err
		}
		res.Removed = append(res.Removed, fingerprint)
	default:
		return nil, fmt.Errorf("say which machine to sign out, or ask for all of them")
	}

	n, err := c.store.InvalidateEnrollCodes(ctx, user, now)
	if err != nil {
		return nil, err
	}
	res.CodesInvalidated = n

	c.store.Event(ctx, 0, "unenroll",
		fmt.Sprintf("%s (%d key(s), %d code(s))", user, len(res.Removed), n), now)
	c.log.Info("unenrolled", "user", user,
		"keys_removed", len(res.Removed), "codes_invalidated", n)

	// Revoke the credentials open sessions are holding. A session token
	// outliving the key that authenticated it would mean an account signed
	// out of a public computer could still call the API from it until the
	// token expired -- which is the exact situation signing out is for.
	//
	// All of the account's, not just the signed-out machine's: a token is
	// not tied to a key, and erring towards revoking too much costs a
	// re-login while erring the other way leaves access behind.
	if err := c.store.DeleteSessionTokensFor(ctx, user); err != nil {
		c.log.Warn("could not revoke session credentials", "user", user, "err", err)
	}

	// Close any session already open on the machines just signed out. Without
	// this, revoking a key would only prevent the *next* login, leaving the
	// connection somebody was worried about running.
	if c.onRevoke != nil {
		if all {
			c.onRevoke(user, "")
		} else {
			for _, fp := range res.Removed {
				c.onRevoke(user, fp)
			}
		}
	}
	return res, nil
}

// OwnerOfKey finds which account a fingerprint belongs to, so an admin can
// sign a machine out knowing only the fingerprint they saw in a listing.
func (c *Controller) OwnerOfKey(ctx context.Context, fingerprint string) (string, error) {
	keys, err := c.store.UserKeys(ctx, "")
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Fingerprint == fingerprint {
			return k.User, nil
		}
	}
	return "", fmt.Errorf("no key with fingerprint %s", fingerprint)
}
