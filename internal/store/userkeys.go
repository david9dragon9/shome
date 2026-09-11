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

// SSH public keys registered against a shome account.
//
// This is what makes "an account the admin created can log in" true without
// creating an OS user for anybody. The login node authenticates a connection
// by looking the offered key up here, so the set of people who can reach the
// cluster is exactly the set of accounts an admin has created and given a key
// to -- managed in one place, revoked in one place.
//
// Keys are stored in full rather than hashed, unlike tokens: a public key is
// public by construction, and keeping it lets an admin see and audit what is
// authorised. The fingerprint is stored alongside for lookup and display.

const userKeysSchema = `
CREATE TABLE IF NOT EXISTS user_keys (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user        TEXT NOT NULL,
  fingerprint TEXT NOT NULL UNIQUE,
  public_key  TEXT NOT NULL,
  comment     TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  last_used   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS user_keys_user ON user_keys(user);
CREATE INDEX IF NOT EXISTS user_keys_fp ON user_keys(fingerprint);
`

// UserKey is one authorised public key.
type UserKey struct {
	ID          int64
	User        string
	Fingerprint string
	PublicKey   string
	Comment     string
	CreatedAt   time.Time
	LastUsed    time.Time
}

// AddUserKey authorises a public key for an account.
//
// The account must already exist. Without that check a typo in the name would
// silently create an authorisation belonging to nobody, which would then never
// work and never explain why.
func (s *Store) AddUserKey(ctx context.Context, user, fingerprint, pubkey, comment string, now time.Time) error {
	if _, err := s.UserByName(ctx, user); err != nil {
		return fmt.Errorf("no such user %q", user)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_keys (user,fingerprint,public_key,comment,created_at) VALUES (?,?,?,?,?)`,
		user, fingerprint, pubkey, comment, ms(now))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			// Which account holds it is deliberately not revealed: an admin
			// can look it up, and the message would otherwise disclose one
			// user's key registration to another.
			return fmt.Errorf("that key is already registered")
		}
		return err
	}
	s.Event(ctx, 0, "user_key_add", user+" "+fingerprint, now)
	return nil
}

// UserByKeyFingerprint resolves an offered key to its account.
//
// Returns the account only if it exists and is not disabled, so quarantining
// somebody locks them out of SSH as well as the API -- one revocation, not two.
func (s *Store) UserByKeyFingerprint(ctx context.Context, fingerprint string) (*User, error) {
	var name string
	err := s.db.QueryRowContext(ctx,
		`SELECT user FROM user_keys WHERE fingerprint=?`, fingerprint).Scan(&name)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("key is not authorised")
	}
	if err != nil {
		return nil, err
	}
	u, err := s.UserByName(ctx, name)
	switch {
	case errors.Is(err, ErrAccountDisabled):
		// Passed through rather than flattened into "key is not authorised".
		// The key is authorised; the account is suspended, which is a
		// different diagnosis and the only one an admin can act on.
		return nil, err
	case err != nil:
		// The key's account no longer exists, so the key really is not
		// authorised -- and saying no more than that is right here, because
		// an offered key is unauthenticated input.
		return nil, fmt.Errorf("key is not authorised")
	}
	return u, nil
}

// TouchUserKey records that a key was just used to log in.
func (s *Store) TouchUserKey(ctx context.Context, fingerprint string, now time.Time) {
	s.db.ExecContext(ctx, `UPDATE user_keys SET last_used=? WHERE fingerprint=?`, ms(now), fingerprint)
}

// UserKeys lists the keys authorised for an account, or for everyone when
// user is empty.
func (s *Store) UserKeys(ctx context.Context, user string) ([]UserKey, error) {
	q := `SELECT id,user,fingerprint,public_key,comment,created_at,last_used FROM user_keys`
	var args []any
	if user != "" {
		q += ` WHERE user=?`
		args = append(args, user)
	}
	q += ` ORDER BY user, id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserKey
	for rows.Next() {
		var k UserKey
		var created, used int64
		if err := rows.Scan(&k.ID, &k.User, &k.Fingerprint, &k.PublicKey, &k.Comment, &created, &used); err != nil {
			return nil, err
		}
		k.CreatedAt = time.UnixMilli(created)
		if used > 0 {
			k.LastUsed = time.UnixMilli(used)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteUserKey revokes one key by fingerprint.
func (s *Store) DeleteUserKey(ctx context.Context, fingerprint string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_keys WHERE fingerprint=?`, fingerprint)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no key with fingerprint %s", fingerprint)
	}
	s.Event(ctx, 0, "user_key_remove", fingerprint, now)
	return nil
}

// DeleteUserKeys revokes every key for an account, used when it is deleted.
//
// Without this a deleted account's keys would linger, and recreating the name
// later would silently re-authorise whoever held them.
func (s *Store) DeleteUserKeys(ctx context.Context, user string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_keys WHERE user=?`, user)
	return err
}

// Enrollment codes: how somebody registers their own key the first time.
//
// Requiring an admin to collect a public key out of band before anyone can
// log in is a real obstacle -- it is not how ssh normally feels, and it puts a
// manual step between creating an account and using it. A code the admin hands
// over once, which the person redeems on their first connection, removes that
// step without weakening anything: the code is single-use, short-lived, and
// buys exactly one thing, the registration of one key against one account.
//
// Deliberately not the account's API token. That token is long-lived and
// grants everything the account can do; handing it to somebody so they can
// log in would mean the weakest part of onboarding is also the most powerful
// credential in it.

const enrollSchema = `
CREATE TABLE IF NOT EXISTS enroll_codes (
  hash       TEXT PRIMARY KEY,
  user       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER NOT NULL DEFAULT 0
);
`

// EnrollTTL is how long an enrollment code stays valid. Long enough to send
// someone a message and have them act on it; short enough that a code left in
// a chat log is not a standing invitation.
const EnrollTTL = 60 * time.Minute

// CreateEnrollCode mints a single-use code that lets user register one key.
func (s *Store) CreateEnrollCode(ctx context.Context, user string, now time.Time) (string, error) {
	// An admin asking for a code, so the reason is theirs to see: minting one
	// for a quarantined account is refused because of the quarantine, not
	// because the account is missing.
	if _, err := s.UserByName(ctx, user); err != nil {
		if errors.Is(err, ErrAccountDisabled) {
			return "", err
		}
		return "", fmt.Errorf("no such user %q", user)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	// Shorter than an API token because a person has to type or paste it into
	// a prompt, and it is single-use and expires within the hour.
	code := base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO enroll_codes (hash,user,created_at,expires_at) VALUES (?,?,?,?)`,
		hashToken(code), user, ms(now), ms(now.Add(EnrollTTL)))
	if err != nil {
		return "", err
	}
	s.Event(ctx, 0, "enroll_code_created", user, now)
	return code, nil
}

// RedeemEnrollCode consumes a code and returns the account it belongs to.
//
// The UPDATE is the atomic step: it matches only a row that is unused and
// unexpired, so two people racing with the same code cannot both succeed.
func (s *Store) RedeemEnrollCode(ctx context.Context, code string, now time.Time) (string, error) {
	h := hashToken(code)
	res, err := s.db.ExecContext(ctx,
		`UPDATE enroll_codes SET used_at=? WHERE hash=? AND used_at=0 AND expires_at>?`,
		ms(now), h, ms(now))
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Deliberately one message for every failure mode. Distinguishing
		// "expired" from "unknown" tells someone guessing codes that they got
		// one right.
		return "", fmt.Errorf("that enrollment code is not valid")
	}
	var user string
	if err := s.db.QueryRowContext(ctx,
		`SELECT user FROM enroll_codes WHERE hash=?`, h).Scan(&user); err != nil {
		return "", err
	}
	// Deliberately NOT distinguished, unlike every other lookup of an
	// account: a code is offered by somebody not yet authenticated as
	// anyone, so "that account is suspended" would answer a question they
	// have not earned. Missing, suspended and never-existed are one answer.
	if _, err := s.UserByName(ctx, user); err != nil {
		return "", fmt.Errorf("that enrollment code is not valid")
	}
	return user, nil
}

// InvalidateEnrollCodes spends every outstanding code for an account.
//
// Called when somebody unenrols. The case that motivates it: a person signs in
// on a public computer, then signs out -- and any code they were issued to get
// there must die with the session, or it is left behind for whoever sits down
// next. Marking them used rather than deleting keeps the audit trail of what
// was issued.
func (s *Store) InvalidateEnrollCodes(ctx context.Context, user string, now time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE enroll_codes SET used_at=? WHERE user=? AND used_at=0`, ms(now), user)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.Event(ctx, 0, "enroll_codes_invalidated", fmt.Sprintf("%s (%d)", user, n), now)
	}
	return int(n), nil
}

// UserKeyByFingerprint returns one key if it belongs to user.
//
// The ownership check is the point: unenrolling is self-service, so without it
// a caller could revoke somebody else's machine by naming its fingerprint.
func (s *Store) UserKeyByFingerprint(ctx context.Context, user, fingerprint string) (*UserKey, error) {
	keys, err := s.UserKeys(ctx, user)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k.Fingerprint == fingerprint {
			return &k, nil
		}
	}
	return nil, fmt.Errorf("no key %s belongs to %s", fingerprint, user)
}

// PendingCode is an issued enrollment code that has not been used yet.
//
// The code itself is stored hashed and cannot be shown again, so it is
// identified by a short handle taken from that hash. That is enough to say
// "cancel that one" without the listing itself becoming a place to read codes
// out of -- which would make `key list` as sensitive as the codes are.
type PendingCode struct {
	ID        string
	User      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// codeHandleLen is how much of the hash identifies a code in listings. Twelve
// base64 characters is ~72 bits, far beyond collision range for the handful of
// codes a cluster has outstanding, and short enough to retype.
const codeHandleLen = 12

// PendingCodes lists unused, unexpired codes, for user or for everyone.
func (s *Store) PendingCodes(ctx context.Context, user string, now time.Time) ([]PendingCode, error) {
	q := `SELECT hash,user,created_at,expires_at FROM enroll_codes WHERE used_at=0 AND expires_at>?`
	args := []any{ms(now)}
	if user != "" {
		q += ` AND user=?`
		args = append(args, user)
	}
	q += ` ORDER BY user, created_at`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingCode
	for rows.Next() {
		var hash string
		var created, expires int64
		if err := rows.Scan(&hash, &user, &created, &expires); err != nil {
			return nil, err
		}
		out = append(out, PendingCode{
			ID:        handleOf(hash),
			User:      user,
			CreatedAt: time.UnixMilli(created),
			ExpiresAt: time.UnixMilli(expires),
		})
	}
	return out, rows.Err()
}

func handleOf(hash string) string {
	if len(hash) > codeHandleLen {
		return hash[:codeHandleLen]
	}
	return hash
}

// InvalidateCodeByHandle cancels one outstanding code.
//
// Returns the account it belonged to, so a caller can report what it did
// without having to look it up separately.
func (s *Store) InvalidateCodeByHandle(ctx context.Context, handle string, now time.Time) (string, error) {
	if handle == "" {
		return "", fmt.Errorf("no code given")
	}
	var hash, user string
	err := s.db.QueryRowContext(ctx,
		`SELECT hash,user FROM enroll_codes WHERE used_at=0 AND expires_at>? AND substr(hash,1,?)=?`,
		ms(now), codeHandleLen, handle).Scan(&hash, &user)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no outstanding code %q", handle)
	}
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE enroll_codes SET used_at=? WHERE hash=?`, ms(now), hash); err != nil {
		return "", err
	}
	s.Event(ctx, 0, "enroll_code_invalidated", user+" "+handle, now)
	return user, nil
}
