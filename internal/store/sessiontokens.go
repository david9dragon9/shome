package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// Session tokens: a credential that exists for the length of one login
// session and no longer.
//
// # Why a third kind of token
//
// An interactive session needs to call the cluster API as the person using
// it -- `shome fs du`, `sbatch`, `squeue` all go through it. None of the
// credentials shome already has will do:
//
//   - The admin token would make every logged-in user an administrator.
//   - The account's own API token is stored only as a hash, deliberately, so
//     it cannot be handed out; and re-issuing it to obtain one would sign the
//     account out of every other machine.
//
// So the login node mints a token scoped to that account, valid for hours
// rather than forever, and deletes it when the session ends. If it leaks, it
// leaks the access its owner already had, for a bounded time.
//
// The session id is recorded so that signing a machine out, or quarantining
// an account, can revoke live sessions immediately rather than at expiry.

const sessionTokenSchema = `
CREATE TABLE IF NOT EXISTS session_tokens (
  -- Hashed like every other credential here: a database read must not yield
  -- something usable.
  token_hash TEXT PRIMARY KEY,
  user       TEXT NOT NULL,
  session    TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS session_tokens_user ON session_tokens(user);
CREATE INDEX IF NOT EXISTS session_tokens_session ON session_tokens(session);
`

// CreateSessionToken issues a token for one login session.
func (s *Store) CreateSessionToken(ctx context.Context, user, session string,
	ttl time.Duration, now time.Time) (string, error) {

	if user == "" || session == "" {
		return "", fmt.Errorf("a session token needs an account and a session id")
	}
	if ttl <= 0 {
		return "", fmt.Errorf("a session token needs a lifetime")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	// Prefixed so it is recognisable in a log or a bug report as a
	// short-lived session credential rather than an account's real token.
	tok := "shs-" + user + "." + base64.RawURLEncoding.EncodeToString(raw)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO session_tokens (token_hash,user,session,expires_at,created_at)
		 VALUES (?,?,?,?,?)`,
		hashToken(tok), user, session, ms(now.Add(ttl)), ms(now)); err != nil {
		return "", err
	}
	return tok, nil
}

// UserBySessionToken resolves a session token to its account.
//
// Expiry is enforced on read rather than only by a sweep: a sweep that has
// not run yet must not leave an expired credential working.
func (s *Store) UserBySessionToken(ctx context.Context, tok string, now time.Time) (*User, error) {
	var user string
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT user,expires_at FROM session_tokens WHERE token_hash=?`,
		hashToken(tok)).Scan(&user, &exp)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("unknown session token")
		}
		return nil, err
	}
	if ms(now) > exp {
		return nil, fmt.Errorf("this session's credential has expired; log in again")
	}
	// Looked up freshly rather than cached with the token, so an account
	// disabled or deleted mid-session stops working at once.
	return s.UserByName(ctx, user)
}

// DeleteSessionTokens revokes every token issued for one session.
func (s *Store) DeleteSessionTokens(ctx context.Context, session string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM session_tokens WHERE session=?`, session)
	return err
}

// DeleteSessionTokensFor revokes every live session credential of an account.
//
// Called when an account is quarantined, deleted, or signed out everywhere:
// withdrawing an account's access has to withdraw the credentials its open
// sessions are holding, or "signed out" would mean "signed out at some point
// in the next few hours".
func (s *Store) DeleteSessionTokensFor(ctx context.Context, user string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM session_tokens WHERE user=?`, user)
	return err
}

// SweepSessionTokens removes expired rows.
func (s *Store) SweepSessionTokens(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM session_tokens WHERE expires_at < ?`, ms(now))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountSessionTokens reports how many live session credentials exist, for the
// dashboard and for tests.
func (s *Store) CountSessionTokens(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_tokens WHERE expires_at >= ?`, ms(now)).Scan(&n)
	return n, err
}

// DeleteFinishedJobTokens removes the credentials of jobs that have ended.
//
// A job's credential is named for the job -- see ctl.jobSession -- so the
// two can be matched without a column of their own. Anything whose job is
// no longer pending or running has no business holding one: the work is
// over, and what is left is a credential in a dead process's environment.
//
// Deliberately keyed on the job's state rather than on being told. A job
// ends in eight different places in the controller, and a credential left
// behind because one of them forgot is not something anybody would notice.
func (s *Store) DeleteFinishedJobTokens(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, `
	  DELETE FROM session_tokens
	  WHERE session LIKE 'job-%'
	    AND CAST(substr(session, 5) AS INTEGER) IN (
	          SELECT id FROM jobs WHERE state NOT IN (?, ?))`,
		string(job.Pending), string(job.Running))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
