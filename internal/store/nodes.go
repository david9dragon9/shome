package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
)

// NodeState is a node's availability from the controller's point of view.
type NodeState string

const (
	NodeUp    NodeState = "UP"    // heartbeating, accepting work
	NodeDown  NodeState = "DOWN"  // missed heartbeats
	NodeDrain NodeState = "DRAIN" // reachable but accepting no new work
)

// Node is a registered node agent.
type Node struct {
	Name string
	// Addr is the address other agents can reach this node on. Peer-to-peer
	// fabrics need it; the controller's own view of the connection is not
	// enough, because that address is only meaningful from the controller.
	Addr     string
	Caps     platform.Capabilities
	State    NodeState
	Reason   string
	LastSeen time.Time
	JoinedAt time.Time
}

// Online reports whether the node may be given new work.
func (n *Node) Online() bool { return n.State == NodeUp }

const nodeSchema = `
CREATE TABLE IF NOT EXISTS nodes (
  name       TEXT PRIMARY KEY,
  addr       TEXT NOT NULL DEFAULT '',
  caps       TEXT NOT NULL DEFAULT '{}',
  state      TEXT NOT NULL,
  reason     TEXT NOT NULL DEFAULT '',
  last_seen  INTEGER NOT NULL DEFAULT 0,
  joined_at  INTEGER NOT NULL DEFAULT 0
);

-- Join tokens are single-use and short-lived: a token is what a human copies
-- between machines, so it is what leaks into shell history and screenshots.
-- Only a hash is stored, so a database read does not yield usable tokens.
CREATE TABLE IF NOT EXISTS join_tokens (
  hash       TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER NOT NULL DEFAULT 0,
  used_by    TEXT NOT NULL DEFAULT ''
);
`

// CreateJoinToken mints a single-use token valid for ttl.
func (s *Store) CreateJoinToken(ctx context.Context, ttl time.Duration, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO join_tokens (hash,created_at,expires_at) VALUES (?,?,?)`,
		hashToken(tok), ms(now), ms(now.Add(ttl)))
	if err != nil {
		return "", err
	}
	return tok, nil
}

// RedeemJoinToken consumes a token, returning an error if it is unknown,
// expired, or already used.
//
// The UPDATE is the atomic step: it only matches a row that is unused and
// unexpired, so two agents racing with the same token cannot both succeed.
func (s *Store) RedeemJoinToken(ctx context.Context, tok, nodeName string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE join_tokens SET used_at=?, used_by=?
		 WHERE hash=? AND used_at=0 AND expires_at>?`,
		ms(now), nodeName, hashToken(tok), ms(now))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		return nil
	}
	// Distinguish the failure modes for a useful error, without leaking
	// whether an unknown token merely expired.
	var usedAt, expiresAt int64
	err = s.db.QueryRowContext(ctx, `SELECT used_at,expires_at FROM join_tokens WHERE hash=?`,
		hashToken(tok)).Scan(&usedAt, &expiresAt)
	switch {
	case err == sql.ErrNoRows:
		return fmt.Errorf("join token is not valid")
	case err != nil:
		return err
	case usedAt != 0:
		return fmt.Errorf("join token was already used")
	default:
		return fmt.Errorf("join token has expired")
	}
}

func hashToken(tok string) string {
	sum := sha256sum([]byte(tok))
	return base64.RawURLEncoding.EncodeToString(sum)
}

// ConstantTimeEqual is used where a token comparison is unavoidable.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// UpsertNode registers or refreshes a node.
func (s *Store) UpsertNode(ctx context.Context, name, addr string, caps platform.Capabilities, now time.Time) error {
	cb, _ := json.Marshal(caps)
	_, err := s.db.ExecContext(ctx, `
	  INSERT INTO nodes (name,addr,caps,state,last_seen,joined_at) VALUES (?,?,?,?,?,?)
	  ON CONFLICT(name) DO UPDATE SET
	    caps=excluded.caps,
	    addr=CASE WHEN excluded.addr!='' THEN excluded.addr ELSE nodes.addr END,
	    last_seen=excluded.last_seen,
	    -- A drained node stays drained across an agent restart: the owner's
	    -- pause must survive a reboot, or it is not a pause.
	    state=CASE WHEN nodes.state='DRAIN' THEN 'DRAIN' ELSE 'UP' END,
	    -- Clear the reason when a node comes back. Otherwise a node that
	    -- briefly went DOWN keeps "missed heartbeats" attached to it forever,
	    -- which reads as an ongoing fault rather than a healed one. A DRAIN
	    -- reason is kept: that one is still true.
	    reason=CASE WHEN nodes.state='DRAIN' THEN nodes.reason ELSE '' END`,
		name, addr, string(cb), string(NodeUp), ms(now), ms(now))
	return err
}

func (s *Store) Heartbeat(ctx context.Context, name string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET last_seen=?, state=CASE WHEN state='DRAIN' THEN 'DRAIN' ELSE 'UP' END WHERE name=?`,
		ms(now), name)
	return err
}

func (s *Store) SetNodeState(ctx context.Context, name string, st NodeState, reason string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE nodes SET state=?, reason=? WHERE name=?`, string(st), reason, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such node %q", name)
	}
	return nil
}

func (s *Store) Nodes(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,addr,caps,state,reason,last_seen,joined_at FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		var n Node
		var caps string
		var seen, joined int64
		if err := rows.Scan(&n.Name, &n.Addr, &caps, &n.State, &n.Reason, &seen, &joined); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(caps), &n.Caps)
		n.LastSeen, n.JoinedAt = unms(seen), unms(joined)
		out = append(out, &n)
	}
	return out, rows.Err()
}

// ExpireNodes marks nodes DOWN once they miss heartbeats for longer than
// timeout. Home machines sleep and move networks constantly, so this is a
// routine transition, not an alarm.
func (s *Store) ExpireNodes(ctx context.Context, timeout time.Duration, now time.Time) ([]string, error) {
	cutoff := ms(now.Add(-timeout))
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM nodes WHERE state!='DOWN' AND state!='DRAIN' AND last_seen < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		s.SetNodeState(ctx, n, NodeDown, "missed heartbeats")
	}
	return names, nil
}

// LostNodeReason marks a job failed because its node stayed unreachable. The
// controller matches on it to recognise a failure it may later be able to
// correct, when the node comes back and reports what actually happened.
const LostNodeReason = "was unreachable too long; not restarted"

// UnreachableReason is shown while a node is missing but its jobs are still
// presumed to be running there.
const UnreachableReason = "node unreachable; job presumed still running"

// MarkJobsUnreachable annotates jobs on a node that has stopped heartbeating,
// WITHOUT changing their state.
//
// A node dropping off and a job dying are different events on different
// timescales. The agent does not kill work when it loses the controller, so a
// laptop that wanders off Wi-Fi is almost always still computing. Killing or
// requeueing at this point would destroy work that is fine; the only correct
// immediate action is to stop sending the node new work and say plainly that
// we cannot see it.
func (s *Store) MarkJobsUnreachable(ctx context.Context, node string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET reason=? WHERE state='RUNNING' AND node=? AND reason!=?`,
		UnreachableReason, node, UnreachableReason)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ClearUnreachable removes that annotation when the node comes back.
func (s *Store) ClearUnreachable(ctx context.Context, node string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET reason='' WHERE state='RUNNING' AND node=? AND reason=?`,
		node, UnreachableReason)
	return err
}

// GiveUpOnLostJobs is the deadline: jobs on a node that has been unreachable
// for longer than grace are finally written off.
//
// Separate from, and much longer than, the heartbeat timeout. The job is only
// abandoned once the node has been gone long enough that it is unlikely to be
// making progress -- or, more precisely, long enough that continuing to wait
// serves nobody. Only jobs that opted into --requeue are re-run; the rest fail,
// because re-running work that already had side effects is not safe.
func (s *Store) GiveUpOnLostJobs(ctx context.Context, grace time.Duration, now time.Time) (requeued, failed int, err error) {
	cutoff := ms(now.Add(-grace))
	rows, err := s.db.QueryContext(ctx, `
	  SELECT j.id, j.requeue, j.node
	  FROM jobs j JOIN nodes n ON n.name = j.node
	  WHERE j.state='RUNNING' AND n.state='DOWN' AND n.last_seen < ?`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	type entry struct {
		id   int64
		req  bool
		node string
	}
	var jobs []entry
	for rows.Next() {
		var e entry
		var r int
		if err := rows.Scan(&e.id, &r, &e.node); err != nil {
			rows.Close()
			return 0, 0, err
		}
		e.req = r != 0
		jobs = append(jobs, e)
	}
	rows.Close()

	for _, j := range jobs {
		if j.req {
			if _, err = s.db.ExecContext(ctx, `
			  UPDATE jobs SET state='PENDING', node='', start_at=0,
			                  reason='requeued: node ' || ? || ' never came back'
			  WHERE id=?`, j.node, j.id); err != nil {
				return requeued, failed, err
			}
			requeued++
			continue
		}
		if _, err = s.db.ExecContext(ctx, `
		  UPDATE jobs SET state=?, exit_code=-1, end_at=?,
		                  reason='node ' || ? || ' ' || ?
		                         || ' (pass --requeue if this job is safe to re-run from the start)'
		  WHERE id=?`, string(job.Failed), ms(now), j.node, LostNodeReason, j.id); err != nil {
			return requeued, failed, err
		}
		failed++
	}
	return requeued, failed, nil
}

// NodeByName returns one node's registry entry.
func (s *Store) NodeByName(ctx context.Context, name string) (*Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name,addr,caps,state,reason,last_seen,joined_at FROM nodes WHERE name=?`, name)
	var n Node
	var caps string
	var seen, joined int64
	if err := row.Scan(&n.Name, &n.Addr, &caps, &n.State, &n.Reason, &seen, &joined); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("no such node %q", name)
		}
		return nil, err
	}
	json.Unmarshal([]byte(caps), &n.Caps)
	n.LastSeen, n.JoinedAt = unms(seen), unms(joined)
	return &n, nil
}

// JoinTokenValid reports whether a token is known, unexpired and unredeemed,
// without consuming it.
//
// The bootstrap service uses this to authorise a binary download: the token in
// the URL is what proves the caller was invited, and it must still be usable
// afterwards because the actual join is a separate request.
func (s *Store) JoinTokenValid(ctx context.Context, tok string, now time.Time) bool {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM join_tokens WHERE hash=? AND used_at=0 AND expires_at>?`,
		hashToken(tok), ms(now)).Scan(&n)
	return err == nil && n == 1
}

// DeleteNode forgets a machine entirely.
//
// Used when a node is removed from the cluster for good, so it stops appearing
// in sinfo as permanently DOWN. Job history is deliberately left alone: it
// records what actually ran and where, and rewriting it because a machine left
// would make the accounting a lie.
func (s *Store) DeleteNode(ctx context.Context, name string) error {
	var running int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE node=? AND state IN ('RUNNING','SUSPENDED')`,
		name).Scan(&running)
	if err != nil {
		return err
	}
	if running > 0 {
		return fmt.Errorf("node %q still has %d running job(s); drain it first", name, running)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE name=?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such node %q", name)
	}
	return nil
}

// Forgetting a machine, and making it stick.
//
// Deleting the row is not enough on its own: the machine is still running,
// still holds a valid certificate, and its next heartbeat -- three seconds
// later -- recreates the row. An admin who clicked "forget" watched the
// node vanish and come back, which reads as the button not working.
//
// So a removal is remembered. A machine whose name is in here is refused
// service and told to stop, until somebody adds it again with a join token,
// which is what deciding to have it back looks like.

// ForgetNode records that a machine was removed from the cluster.
func (s *Store) ForgetNode(ctx context.Context, name string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO forgotten_nodes (name,at) VALUES (?,?)
		 ON CONFLICT(name) DO UPDATE SET at=excluded.at`, name, ms(now))
	return err
}

// NodeForgotten reports whether this machine was removed and has not been
// added back.
func (s *Store) NodeForgotten(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM forgotten_nodes WHERE name=?`, name).Scan(&n)
	return n > 0, err
}

// UnforgetNode clears the record, for a machine that has joined again.
func (s *Store) UnforgetNode(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM forgotten_nodes WHERE name=?`, name)
	return err
}
