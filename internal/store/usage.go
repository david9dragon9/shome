package store

import (
	"context"
	"time"
)

// Historical resource consumption, for fair-share priority.
//
// Derived from the job records rather than accumulated in a counter. A
// counter drifts: every crash, requeue and manual edit is a chance to lose
// sync, and a fair-share number that has quietly diverged from what actually
// ran is worse than none, because nobody can argue with it. Deriving is
// self-correcting, and the same choice disk accounting makes.

// JobUsage is one job's contribution to its account's usage.
type JobUsage struct {
	User     string
	CPUs     int
	GPUs     int
	MemBytes int64
	// Start and End bound the time it held those resources. End is zero for
	// a job still running.
	Start time.Time
	End   time.Time
}

// UsageSince returns every job that held resources at or after `since`,
// including ones still running.
//
// Running jobs are included because usage has to accrue as it happens: an
// account that launched ten week-long jobs an hour ago has taken the cluster,
// and a fair-share number that shows zero until they finish would let them do
// it again tomorrow.
//
// The window is a cutoff rather than everything, because a contribution older
// than several half-lives is multiplied by so small a number that reading it
// costs more than it changes.
func (s *Store) UsageSince(ctx context.Context, since time.Time) ([]JobUsage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT user, cpus, gpus, mem_bytes, start_at, end_at
		FROM jobs
		WHERE start_at > 0 AND (end_at = 0 OR end_at >= ?)`, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []JobUsage
	for rows.Next() {
		var u JobUsage
		var start, end int64
		if err := rows.Scan(&u.User, &u.CPUs, &u.GPUs, &u.MemBytes, &start, &end); err != nil {
			return nil, err
		}
		u.Start, u.End = unms(start), unms(end)
		out = append(out, u)
	}
	return out, rows.Err()
}
