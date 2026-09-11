package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAccountDisabled is returned by every account lookup when the account
// exists, and its credential is genuine, but it has been suspended.
//
// A distinct error because the two refusals are different answers to the
// caller. "This credential is not one of ours" and "this credential is yours
// and your account is suspended" call for different things from whoever reads
// it: the first sends somebody looking for a token they have not lost, and
// only the second is true. Every lookup still returns a non-nil error, so a
// caller that does not care to distinguish refuses either way.
var ErrAccountDisabled = errors.New("account is suspended")

// Role determines what a caller may do.
type Role string

const (
	// RoleAdmin is cluster-wide: every job, every node, user management.
	// It still cannot override a node owner's policy -- that asymmetry is the
	// whole sovereignty guarantee, so it is enforced in code, not documented.
	RoleAdmin Role = "admin"
	// RoleOperator can act on running work -- kill, hold, requeue, drain --
	// but cannot change who exists or what they may have. Day-to-day cluster
	// babysitting should not require the credential that can mint accounts.
	RoleOperator Role = "operator"
	// RoleUser sees and controls only their own jobs.
	RoleUser Role = "user"
)

// Rank orders roles for permission checks.
func (r Role) Rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleOperator:
		return 2
	case RoleUser:
		return 1
	}
	return 0
}

// AtLeast reports whether this role includes the powers of another.
func (r Role) AtLeast(other Role) bool { return r.Rank() >= other.Rank() }

// ValidRole parses a role name.
func ValidRole(s string) (Role, bool) {
	switch Role(s) {
	case RoleAdmin, RoleOperator, RoleUser:
		return Role(s), true
	}
	return "", false
}

// User is a shome account.
//
// Deliberately NOT an OS account: creating those on macOS cannot be reversed
// without Recovery Mode (see docs/design-notes.md). Identity is shome's own, so isolation and
// access control are enforced by shome rather than by POSIX ownership.
type User struct {
	Name       string
	Role       Role
	QuotaBytes int64 // 0 means unlimited
	UsedBytes  int64
	Disabled   bool
	CreatedAt  time.Time
}

const userSchema = `
CREATE TABLE IF NOT EXISTS users (
  name        TEXT PRIMARY KEY,
  role        TEXT NOT NULL DEFAULT 'user',
  -- Only a hash is stored. A database read must not yield usable credentials.
  token_hash  TEXT NOT NULL,
  quota_bytes INTEGER NOT NULL DEFAULT 0,
  disabled    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS users_token ON users(token_hash);
`

// CreateUser adds an account and returns its API token, which is shown once
// and never recoverable afterwards.
func (s *Store) CreateUser(ctx context.Context, name string, role Role, quota int64, now time.Time) (string, error) {
	if err := validUserName(name); err != nil {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := name + "." + base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (name,role,token_hash,quota_bytes,created_at) VALUES (?,?,?,?,?)`,
		name, string(role), hashToken(tok), quota, ms(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", fmt.Errorf("user %q already exists", name)
		}
		return "", err
	}
	return tok, nil
}

// validUserName keeps names usable as path components: a user's storage lives
// in a directory named after them, so "../" would be an escape.
func validUserName(name string) error {
	if name == "" {
		return fmt.Errorf("user name is required")
	}
	if len(name) > 32 {
		return fmt.Errorf("user name is too long (max 32)")
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return fmt.Errorf("user name %q may only contain letters, digits, '-' and '_'", name)
		}
	}
	return nil
}

// UserByToken resolves an API token to its account.
func (s *Store) UserByToken(ctx context.Context, tok string) (*User, error) {
	var u User
	var created int64
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT name,role,quota_bytes,disabled,created_at FROM users WHERE token_hash=?`,
		hashToken(tok)).Scan(&u.Name, &u.Role, &u.QuotaBytes, &disabled, &created)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("unknown token")
	}
	if err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	u.CreatedAt = unms(created)
	if u.Disabled {
		return nil, fmt.Errorf("%q: %w", u.Name, ErrAccountDisabled)
	}
	return &u, nil
}

// UserByName looks up an account without a token, for admin impersonation.
func (s *Store) UserByName(ctx context.Context, name string) (*User, error) {
	var u User
	var created int64
	var disabled int
	err := s.db.QueryRowContext(ctx,
		`SELECT name,role,quota_bytes,disabled,created_at FROM users WHERE name=?`,
		name).Scan(&u.Name, &u.Role, &u.QuotaBytes, &disabled, &created)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("no such user %q", name)
	}
	if err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	u.CreatedAt = unms(created)
	if u.Disabled {
		return nil, fmt.Errorf("%q: %w", name, ErrAccountDisabled)
	}
	return &u, nil
}

func (s *Store) Users(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name,role,quota_bytes,disabled,created_at FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		var u User
		var created int64
		var disabled int
		if err := rows.Scan(&u.Name, &u.Role, &u.QuotaBytes, &disabled, &created); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		u.CreatedAt = unms(created)
		out = append(out, &u)
	}
	return out, rows.Err()
}

func (s *Store) SetUserDisabled(ctx context.Context, name string, disabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET disabled=? WHERE name=?`, boolInt(disabled), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such user %q", name)
	}
	return nil
}

// DeleteUser removes an account. Their jobs are left in the history: an audit
// trail that disappears when an account is deleted is not an audit trail.
func (s *Store) DeleteUser(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such user %q", name)
	}
	// Revoke their SSH keys with the account. Leaving them would mean that
	// recreating the same name later silently re-authorises whoever still
	// holds the old key.
	return s.DeleteUserKeys(ctx, name)
}

// CountUsers reports how many accounts exist, for first-run bootstrap.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UserQuota returns a user's limit, or 0 for unlimited.
func (s *Store) UserQuota(ctx context.Context, name string) (int64, error) {
	var q int64
	err := s.db.QueryRowContext(ctx, `SELECT quota_bytes FROM users WHERE name=?`, name).Scan(&q)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no such user %q", name)
	}
	return q, err
}

func (s *Store) SetUserQuota(ctx context.Context, name string, quota int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET quota_bytes=? WHERE name=?`, quota, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such user %q", name)
	}
	return nil
}

// ResetUserToken issues a new API token for an existing account, invalidating
// the old one. Tokens are stored hashed and cannot be recovered, so without
// this a lost token would mean a permanently unreachable account -- including
// the owner's own admin account.
func (s *Store) ResetUserToken(ctx context.Context, name string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := name + "." + base64.RawURLEncoding.EncodeToString(raw)
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET token_hash=? WHERE name=?`, hashToken(tok), name)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("no such user %q", name)
	}
	return tok, nil
}

// UserQoS returns an account's QoS overrides as stored JSON, empty when it has
// none and inherits the cluster defaults entirely.
func (s *Store) UserQoS(ctx context.Context, name string) (string, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT qos FROM users WHERE name=?`, name).Scan(&raw)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no such user %q", name)
	}
	return raw, err
}

// SetUserQoS replaces an account's QoS overrides.
func (s *Store) SetUserQoS(ctx context.Context, name, raw string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET qos=? WHERE name=?`, raw, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such user %q", name)
	}
	return nil
}

// CountActiveForUser counts an account's queued and running jobs, which is
// what a submission limit is measured against.
func (s *Store) CountActiveForUser(ctx context.Context, user string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE user=? AND state IN ('PENDING','RUNNING')`,
		user).Scan(&n)
	return n, err
}

// CountActive counts every queued and running job, for the cluster-wide
// queue-depth limit.
func (s *Store) CountActive(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE state IN ('PENDING','RUNNING')`).Scan(&n)
	return n, err
}
