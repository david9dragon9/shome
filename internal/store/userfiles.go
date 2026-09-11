package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The index of who has what, where.
//
// A file is identified by the pair (node, path) under an account, which is
// what "no shared filesystem" means in practice: the same relative path on two
// machines is two files, each costing its own space. The index exists so a
// user can see and manage all of them from anywhere, and so the cluster can
// enforce a total across machines that no single machine could enforce alone.
//
// Reported by agents rather than written on transfer. A transfer that
// succeeded but whose bookkeeping was lost would leave the index claiming
// space that is free, or worse, free space that is occupied -- and the figure
// a limit is enforced against must come from the filesystem, not from a
// record of what was meant to happen.

const userFilesSchema = `
CREATE TABLE IF NOT EXISTS user_files (
  node       TEXT NOT NULL,
  user       TEXT NOT NULL,
  path       TEXT NOT NULL,
  size       INTEGER NOT NULL,
  mtime      INTEGER NOT NULL,
  PRIMARY KEY (node, user, path)
);
CREATE INDEX IF NOT EXISTS user_files_user ON user_files(user);

CREATE TABLE IF NOT EXISTS user_disk (
  node       TEXT NOT NULL,
  user       TEXT NOT NULL,
  bytes      INTEGER NOT NULL,
  files      INTEGER NOT NULL,
  truncated  INTEGER NOT NULL DEFAULT 0,
  at         INTEGER NOT NULL,
  PRIMARY KEY (node, user)
);
CREATE INDEX IF NOT EXISTS user_disk_user ON user_disk(user);
`

// UserFile is one indexed file.
type UserFile struct {
	Node  string
	User  string
	Path  string
	Size  int64
	MTime time.Time
}

// NodeUsage is one account's total on one machine.
type NodeUsage struct {
	Node      string
	User      string
	Bytes     int64
	Files     int
	Truncated bool
	At        time.Time
}

// ReplaceNodeFiles records what one node reports for one user.
//
// A wholesale replace rather than a merge: the report is the node's complete
// view, so a file it no longer lists has gone, and merging would leave
// deleted files in the index forever.
func (s *Store) ReplaceNodeFiles(ctx context.Context, node, user string,
	files []UserFile, usage NodeUsage, now time.Time) error {

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_files WHERE node=? AND user=?`, node, user); err != nil {
		return err
	}
	if len(files) > 0 {
		st, err := tx.PrepareContext(ctx,
			`INSERT INTO user_files (node,user,path,size,mtime) VALUES (?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, f := range files {
			if _, err := st.ExecContext(ctx, node, user, f.Path, f.Size, ms(f.MTime)); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_disk (node,user,bytes,files,truncated,at) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(node,user) DO UPDATE SET
		   bytes=excluded.bytes, files=excluded.files,
		   truncated=excluded.truncated, at=excluded.at`,
		node, user, usage.Bytes, usage.Files, boolInt(usage.Truncated), ms(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordNodeUsage records one account's totals on one machine without
// touching the file listing.
//
// The two are separable because they answer different questions and are
// trusted differently: the totals are what a limit is enforced against and
// come from the machine measuring its own disk, while the listing is a cache
// for display and for working out what to copy. A report whose walk began
// before a transfer the controller arranged cannot speak for the listing,
// but its totals are still this machine's own measurement and are the best
// figure there is.
func (s *Store) RecordNodeUsage(ctx context.Context, usage NodeUsage, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_disk (node,user,bytes,files,truncated,at) VALUES (?,?,?,?,?,?)
		 ON CONFLICT(node,user) DO UPDATE SET
		   bytes=excluded.bytes, files=excluded.files,
		   truncated=excluded.truncated, at=excluded.at`,
		usage.Node, usage.User, usage.Bytes, usage.Files,
		boolInt(usage.Truncated), ms(now))
	return err
}

// ForgetNodeStorage drops a departed machine's entries.
//
// Called when a node leaves. Leaving them would keep counting space on a
// machine that is gone, so a user would be permanently over a limit they
// cannot get back under.
func (s *Store) ForgetNodeStorage(ctx context.Context, node string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_files WHERE node=?`, node); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_disk WHERE node=?`, node)
	return err
}

// ForgetUserStorage drops a deleted account's entries.
func (s *Store) ForgetUserStorage(ctx context.Context, user string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_files WHERE user=?`, user); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_disk WHERE user=?`, user)
	return err
}

// NoteUserFile records one file in the index straight away.
//
// The index is otherwise built from what each machine reports, every twenty
// seconds, and that is deliberate: a usage figure has to be measured from
// the filesystem rather than accumulated from a record of what was meant to
// happen. This does not change that -- it adds the row for a file that is
// known to exist, because the transfer that put it there has just confirmed
// it, and leaves every usage total alone for the next report to measure.
//
// Without it, a copy followed immediately by anything that reads the
// listing -- `shome fs cp mini:data gpu:` then `shome fs cp gpu:data .` --
// failed with "not in the index" for a file that was demonstrably there.
func (s *Store) NoteUserFile(ctx context.Context, f UserFile) error {
	if f.Node == "" || f.User == "" || f.Path == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
	  INSERT INTO user_files (node,user,path,size,mtime) VALUES (?,?,?,?,?)
	  ON CONFLICT(node,user,path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime`,
		f.Node, f.User, f.Path, f.Size, ms(f.MTime))
	return err
}

// ForgetUserFile drops one file from the index, for the same reason
// NoteUserFile adds one: a move has just confirmed the original is gone.
func (s *Store) ForgetUserFile(ctx context.Context, node, user, path string) error {
	if node == "" || user == "" || path == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM user_files WHERE node=? AND user=? AND path=?`, node, user, path)
	return err
}

// UserDiskByNode returns one account's usage on each machine.
func (s *Store) UserDiskByNode(ctx context.Context, user string) ([]NodeUsage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node,user,bytes,files,truncated,at FROM user_disk WHERE user=? ORDER BY node`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeUsage
	for rows.Next() {
		var u NodeUsage
		var trunc int
		var at int64
		if err := rows.Scan(&u.Node, &u.User, &u.Bytes, &u.Files, &trunc, &at); err != nil {
			return nil, err
		}
		u.Truncated, u.At = trunc != 0, time.UnixMilli(at)
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserDiskTotal is one account's usage summed across every machine.
//
// This is the figure a cluster-wide limit is enforced against. Summed from
// what nodes reported, so a machine that has not checked in recently still
// counts -- its files have not stopped existing because it went quiet.
func (s *Store) UserDiskTotal(ctx context.Context, user string) (bytes int64, files int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes),0), COALESCE(SUM(files),0) FROM user_disk WHERE user=?`,
		user).Scan(&bytes, &files)
	return
}

// AllUserDisk returns every account's per-machine usage, for admin views.
func (s *Store) AllUserDisk(ctx context.Context) ([]NodeUsage, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT node,user,bytes,files,truncated,at FROM user_disk ORDER BY user, node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeUsage
	for rows.Next() {
		var u NodeUsage
		var trunc int
		var at int64
		if err := rows.Scan(&u.Node, &u.User, &u.Bytes, &u.Files, &trunc, &at); err != nil {
			return nil, err
		}
		u.Truncated, u.At = trunc != 0, time.UnixMilli(at)
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserFiles lists an account's indexed files, optionally on one machine and
// under one path prefix.
func (s *Store) UserFiles(ctx context.Context, user, node, prefix string, limit int) ([]UserFile, error) {
	q := `SELECT node,user,path,size,mtime FROM user_files WHERE user=?`
	args := []any{user}
	if node != "" {
		q += ` AND node=?`
		args = append(args, node)
	}
	if prefix != "" {
		q += ` AND path LIKE ?`
		args = append(args, prefix+"%")
	}
	q += ` ORDER BY node, path`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserFile
	for rows.Next() {
		var f UserFile
		var mt int64
		if err := rows.Scan(&f.Node, &f.User, &f.Path, &f.Size, &mt); err != nil {
			return nil, err
		}
		f.MTime = time.UnixMilli(mt)
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindUserFile looks up one file on one machine.
func (s *Store) FindUserFile(ctx context.Context, user, node, path string) (UserFile, error) {
	var f UserFile
	var mt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT node,user,path,size,mtime FROM user_files WHERE user=? AND node=? AND path=?`,
		user, node, path).Scan(&f.Node, &f.User, &f.Path, &f.Size, &mt)
	if err == sql.ErrNoRows {
		return f, fmt.Errorf("%s:%s is not in the index", node, path)
	}
	f.MTime = time.UnixMilli(mt)
	return f, err
}

// PruneNodeUsers drops index entries for accounts a machine no longer holds
// files for.
//
// Without this, an account that deleted its last file on a machine would keep
// being charged for it forever: the machine simply stops mentioning them, and
// silence is not something a per-user update can act on. Only safe to call
// with a machine's complete view -- see Heartbeat.StorageOK.
func (s *Store) PruneNodeUsers(ctx context.Context, node string, keep []string) error {
	q := `DELETE FROM %s WHERE node=?`
	args := []any{node}
	if len(keep) > 0 {
		q += ` AND user NOT IN (?` + strings.Repeat(`,?`, len(keep)-1) + `)`
		for _, u := range keep {
			args = append(args, u)
		}
	}
	for _, table := range []string{"user_files", "user_disk"} {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(q, table), args...); err != nil {
			return err
		}
	}
	return nil
}
