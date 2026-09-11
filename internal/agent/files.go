package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/userfs"
)

// The agent's side of per-user file storage.
//
// Two jobs: report what each account occupies here, so the controller can
// index it and enforce a total across machines; and carry out the file actions
// the controller queues.
//
// The reporting is deliberately from the filesystem rather than from a running
// tally of transfers. A tally can drift -- a failed rename, a file removed by
// something else, an agent restarted mid-copy -- and a limit enforced against
// a drifting number eventually locks somebody out of space that is free, or
// lets them fill a disk that is not.

// FileIndexCap bounds how many entries one account's report carries.
//
// The usage totals are exact regardless; only the listing is capped, and the
// cap is reported so a large tree shows as partial rather than wrong.
//
// It used to be 5,000, which is smaller than a single Python environment: a
// virtualenv with numpy and pandas in it is tens of thousands of files, so
// an account that had installed anything had a truncated listing -- and
// because `shome fs cp` and `shome fs sync` work out what to copy from that
// listing, they quietly stopped seeing files that were plainly there.
//
// The old figure was small because the report rode on every heartbeat, ten
// times a second. It is now sent only when it has been re-measured, so the
// cost is one payload per StorageInterval rather than per tick, and a cap
// that fits a real environment is affordable. A tree larger than this is
// still reported as partial rather than wrong.
const FileIndexCap = 100000

// storeFor is this machine's user storage.
func (a *Agent) storeFor() *userfs.Store { return a.Files }

// StorageReport measures every account's files on this machine, when there
// is something new to say.
//
// Rate-limited two ways. The walk itself is the most expensive thing the
// heartbeat does, and the numbers do not change fast enough to be worth
// doing every few seconds on a machine with a large working set -- so it
// happens at most once per StorageInterval. And a measurement already sent
// is not sent again: heartbeats are ten a second, the listing is large, and
// re-sending an unchanged copy of it a hundred times between measurements
// achieved nothing except to make a small cap on its size look necessary.
//
// A re-measurement that found nothing different is not sent either. On an
// idle machine that is every one of them, and the alternative is a
// multi-megabyte listing every twenty seconds saying exactly what the last
// one said.
//
// The second result says whether this is a report at all. False means "no
// news", which the controller already had to handle -- it is the same answer
// as "I could not measure", and in both cases the right thing is to leave
// the index alone.
func (a *Agent) StorageReport() ([]agentapi.UserStorage, time.Time, bool) {
	a.mu.Lock()
	fresh := time.Since(a.storageAt) < StorageInterval && a.storageOK
	cached := a.storageCache
	cachedStart := a.storageStart
	sent := a.storageSent
	a.mu.Unlock()
	if fresh {
		if sent {
			return nil, time.Time{}, false // already reported; nothing has changed
		}
		a.mu.Lock()
		a.storageSent = true
		a.mu.Unlock()
		return cached, cachedStart, true
	}
	// When the walk began, not when it ended. A file that arrives while it
	// is running may or may not be seen, so the conservative time is the
	// start: the controller then knows this report cannot speak for
	// anything it learned after that.
	start := time.Now()

	st := a.storeFor()
	users, err := st.Users()
	if err != nil {
		// Say nothing rather than resend a view we could not confirm: one
		// failed walk must not make the controller believe this machine has
		// been emptied, and it need not make it rewrite the index either.
		return nil, time.Time{}, false
	}
	out := make([]agentapi.UserStorage, 0, len(users))
	for _, u := range users {
		entries, usage, truncated, err := st.Walk(u, FileIndexCap)
		if err != nil {
			continue
		}
		rep := agentapi.UserStorage{
			User: u, Bytes: usage.Bytes, Files: usage.Files, Truncated: truncated,
		}
		for _, e := range entries {
			rep.Entries = append(rep.Entries, agentapi.FileEntry{
				Path: e.Path, Size: e.Size, MTime: e.MTime.UnixMilli(),
			})
		}
		out = append(out, rep)
	}
	fp := storageFingerprint(out)
	a.mu.Lock()
	// Against the last report actually handed over, which is not the same as
	// the last measurement: invalidating the cache asks for a fresh look,
	// and a fresh look at an unchanged tree is still nothing to say.
	unchanged := a.storageFPSet && a.storageFP == fp
	a.storageCache, a.storageAt, a.storageOK = out, time.Now(), true
	a.storageStart, a.storageSent = start, true
	a.storageFP, a.storageFPSet = fp, true
	a.mu.Unlock()
	if unchanged {
		return nil, time.Time{}, false
	}
	return out, start, true
}

// storageFingerprint summarises a report, so an unchanged one need not be
// sent again.
//
// Over what the controller would store -- each account, each path, size and
// modification time -- because those are exactly the differences worth a
// round trip. Not a cryptographic hash: this decides whether to re-send a
// listing, and the cost of a collision is a listing that stays as it was
// until the next change.
func storageFingerprint(rep []agentapi.UserStorage) uint64 {
	h := fnv.New64a()
	var num [24]byte
	write := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	writeInt := func(v int64) { h.Write(strconv.AppendInt(num[:0], v, 10)); h.Write([]byte{0}) }
	for _, r := range rep {
		write(r.User)
		writeInt(r.Bytes)
		writeInt(int64(r.Files))
		if r.Truncated {
			write("truncated")
		}
		for _, e := range r.Entries {
			write(e.Path)
			writeInt(e.Size)
			writeInt(e.MTime)
		}
	}
	return h.Sum64()
}

// StorageInterval is how often the file index is rebuilt.
const StorageInterval = 20 * time.Second

// InvalidateStorage forces the next report to re-measure.
//
// Called after this agent changes a user's files, so a transfer is reflected
// on the next heartbeat rather than up to StorageInterval later -- the window
// in which a cluster-wide limit would be enforced against a stale total.
func (a *Agent) InvalidateStorage() {
	a.mu.Lock()
	a.storageAt = time.Time{}
	a.storageSent = false
	a.mu.Unlock()
}

// TakeTransferResults returns finished transfer outcomes for the heartbeat.
//
// Held until reported rather than sent directly, for the same reason job
// results are: the controller may be unreachable at the moment a copy
// finishes, and a transfer whose outcome was lost would leave the requester
// waiting forever on something that had already happened.
func (a *Agent) TakeTransferResults() []agentapi.TransferResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.transfers
	a.transfers = nil
	return out
}

func (a *Agent) reportTransfer(r agentapi.TransferResult) {
	a.mu.Lock()
	a.transfers = append(a.transfers, r)
	a.mu.Unlock()
}

// RemoveFile deletes one of a user's files on this machine.
func (a *Agent) RemoveFile(act agentapi.Action) {
	err := a.storeFor().Remove(act.FileUser, act.FilePath, act.Recursive)
	res := agentapi.TransferResult{Transfer: act.Transfer, Kind: string(act.Kind), OK: err == nil}
	if err != nil {
		res.Error = err.Error()
	} else {
		a.InvalidateStorage()
		a.Log.Info("removed a user file", "user", act.FileUser, "path", act.FilePath)
	}
	a.reportTransfer(res)
}

// SendFile uploads one of a user's files to the controller.
func (a *Agent) SendFile(ctx context.Context, c *Client, act agentapi.Action) {
	res := agentapi.TransferResult{Transfer: act.Transfer, Kind: string(act.Kind)}
	rc, size, err := a.storeFor().Open(act.FileUser, act.FilePath)
	if err != nil {
		res.Error = err.Error()
		a.reportTransfer(res)
		return
	}
	defer rc.Close()
	if err := c.UploadUserFile(ctx, act.Transfer, rc); err != nil {
		res.Error = err.Error()
		a.reportTransfer(res)
		return
	}
	res.OK, res.Bytes = true, size
	a.Log.Info("sent a user file", "user", act.FileUser, "path", act.FilePath, "bytes", size)
	a.reportTransfer(res)
}

// RecvFile downloads a file from the controller into a user's storage here.
func (a *Agent) RecvFile(ctx context.Context, c *Client, act agentapi.Action) {
	res := agentapi.TransferResult{Transfer: act.Transfer, Kind: string(act.Kind)}
	rc, err := c.DownloadUserFile(ctx, act.Transfer)
	if err != nil {
		res.Error = err.Error()
		a.reportTransfer(res)
		return
	}
	defer rc.Close()

	// The limit travels with the action: it is the account's remaining
	// cluster-wide allowance as the controller computed it, so a node that
	// cannot see the other machines still refuses to exceed the total.
	n, err := a.storeFor().Put(act.FileUser, act.FilePath, rc, act.Limit)
	if err != nil {
		res.Error = err.Error()
		a.reportTransfer(res)
		return
	}
	res.OK, res.Bytes = true, n
	a.InvalidateStorage()
	a.Log.Info("received a user file", "user", act.FileUser, "path", act.FilePath, "bytes", n)
	a.reportTransfer(res)
}

var _ = fmt.Sprint
