package ctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/store"
)

// Moving a user's files between machines.
//
// Relayed through the controller, in two hops: the source node uploads, then
// the destination node downloads. Not because that is efficient -- it is not,
// every byte crosses the network twice -- but because it is the only topology
// that always works. Agents dial out and cannot be dialled, so a node behind
// NAT or on a network that blocks peer traffic is reachable only over the
// connection it opened. The controller is the one place both ends can always
// reach.
//
// A direct path would be a worthwhile optimisation later, and would have to
// fall back to this whenever it failed. Building the fallback first means a
// copy works everywhere before it works quickly anywhere.

// TransferTTL bounds how long a relayed blob and its record survive.
//
// Generous, because the two hops are separated by however long it takes the
// destination's next heartbeat to arrive, and a node on a slow link should
// not lose a transfer to impatience. Bounded, because the blob is on disk.
const TransferTTL = 30 * time.Minute

// TransferState is where a transfer has got to.
type TransferState string

const (
	TransferSending TransferState = "sending" // waiting for the source to upload
	TransferStaged  TransferState = "staged"  // in hand, waiting for the destination
	TransferDone    TransferState = "done"
	TransferFailed  TransferState = "failed"
)

// Transfer is one file moving between machines.
type Transfer struct {
	ID       int64         `json:"id"`
	User     string        `json:"user"`
	FromNode string        `json:"from_node"`
	FromPath string        `json:"from_path"`
	ToNode   string        `json:"to_node"`
	ToPath   string        `json:"to_path"`
	Move     bool          `json:"move"`
	Size     int64         `json:"size"`
	State    TransferState `json:"state"`
	Error    string        `json:"error,omitempty"`
	At       time.Time     `json:"at"`
}

// Done reports whether the transfer has finished, either way.
func (t *Transfer) Done() bool {
	return t.State == TransferDone || t.State == TransferFailed
}

// transfers tracks in-flight file movements.
//
// In memory: a transfer is worthless after a restart, because the blob it
// referred to may be half-written and the agents involved have forgotten the
// action. Failing them on restart is honest; resuming them would not be.
type transfers struct {
	mu   sync.Mutex
	next int64
	m    map[int64]*Transfer
}

func newTransfers() *transfers { return &transfers{m: map[int64]*Transfer{}} }

// blobPath is where a relayed file waits between hops.
func (c *Controller) blobPath(id int64) string {
	return filepath.Join(c.root, "transfer", fmt.Sprintf("t-%d.bin", id))
}

// StartTransfer authorises a copy or move and queues the first hop.
func (c *Controller) StartTransfer(ctx context.Context, user, fromNode, fromPath,
	toNode, toPath string, move bool) (*Transfer, error) {

	// Both machines must be known, or the transfer would sit queued forever
	// against a name that will never check in. Resolving here also means an
	// admin's label works as a machine name.
	fromNode, err := c.NodeIdentity(ctx, fromNode)
	if err != nil {
		return nil, err
	}
	toNode, err = c.NodeIdentity(ctx, toNode)
	if err != nil {
		return nil, err
	}
	// After resolution, not before: a label and an identity are two names for
	// one machine, so "mini:f -> a-long-hostname:f" is a file onto itself
	// and comparing the typed names would not notice.
	if fromNode == toNode && fromPath == toPath {
		return nil, fmt.Errorf("source and destination are the same file")
	}
	src, ferr := c.store.FindUserFile(ctx, user, fromNode, fromPath)
	if ferr != nil {
		return nil, fmt.Errorf("%w\n\nThe index is built from what each machine reports, so a\n"+
			"file created seconds ago may not be listed yet", ferr)
	}

	// Authorise against the cluster-wide total. A copy costs its size again
	// on the destination, which is the thing users have to understand about a
	// cluster with no shared filesystem -- so it is checked here and said
	// plainly when it fails.
	if !move || fromNode != toNode {
		if err := c.checkDiskHeadroom(ctx, user, src.Size, toNode, toPath); err != nil {
			return nil, err
		}
	}

	c.xfer.mu.Lock()
	c.xfer.next++
	t := &Transfer{
		ID: c.xfer.next, User: user, FromNode: fromNode, FromPath: fromPath,
		ToNode: toNode, ToPath: toPath, Move: move, Size: src.Size,
		State: TransferSending, At: c.now(),
	}
	c.xfer.m[t.ID] = t
	c.xfer.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(c.blobPath(t.ID)), 0o700); err != nil {
		return nil, err
	}
	c.enqueue(fromNode, agentapi.Action{
		Kind: agentapi.ActionFileSend, Transfer: t.ID,
		FileUser: user, FilePath: fromPath,
	})
	c.log.Info("transfer queued", "id", t.ID, "user", user,
		"from", fromNode+":"+fromPath, "to", toNode+":"+toPath, "move", move)
	go c.expireTransfer(t.ID)
	return t, nil
}

// checkDiskHeadroom refuses a transfer that would put an account over its
// cluster-wide limit.
//
// Counts the whole cluster, not the destination machine. A per-machine check
// would let somebody hold ten copies of the same 50 GB file by spreading it
// across ten machines, each of them individually within bounds -- which is
// exactly the hole a cross-machine total exists to close.
func (c *Controller) checkDiskHeadroom(ctx context.Context, user string, add int64,
	toNode, toPath string) error {

	e, err := c.EffectiveQoS(ctx, user)
	if err != nil {
		return err
	}
	if e.MaxDiskMB < 0 {
		return nil // unlimited
	}
	limit := e.MaxDiskMB << 20
	used, _, err := c.store.UserDiskTotal(ctx, user)
	if err != nil {
		return err
	}
	// Overwriting an existing file at the destination reclaims its space, so
	// count only the difference. Without this, replacing a file with itself
	// would be refused on a nearly full account.
	if old, err := c.store.FindUserFile(ctx, user, toNode, toPath); err == nil {
		add -= old.Size
	}
	if used+add > limit {
		return fmt.Errorf(
			"this would put you at %s of your %s cluster-wide storage limit.\n\n"+
				"Storage is per machine and adds up: a copy costs its size again on\n"+
				"the destination. See where it has gone with 'shome fs du'",
			mib(used+add), mib(limit))
	}
	return nil
}

// Headroom is how much more an account may store, cluster-wide.
func (c *Controller) Headroom(ctx context.Context, user string) (int64, error) {
	e, err := c.EffectiveQoS(ctx, user)
	if err != nil {
		return 0, err
	}
	if e.MaxDiskMB < 0 {
		return -1, nil // unlimited
	}
	used, _, err := c.store.UserDiskTotal(ctx, user)
	if err != nil {
		return 0, err
	}
	if rem := (e.MaxDiskMB << 20) - used; rem > 0 {
		return rem, nil
	}
	return 0, nil
}

// StageTransfer accepts the uploaded blob from the source node.
func (c *Controller) StageTransfer(id int64, node string, r io.Reader) error {
	t, err := c.transferFor(id, node, TransferSending)
	if err != nil {
		return err
	}
	f, err := os.Create(c.blobPath(id))
	if err != nil {
		return err
	}
	n, err := io.Copy(f, r)
	cerr := f.Close()
	if err != nil || cerr != nil {
		os.Remove(c.blobPath(id))
		c.failTransfer(id, "relay failed")
		if err == nil {
			err = cerr
		}
		return err
	}

	c.xfer.mu.Lock()
	t.State, t.Size = TransferStaged, n
	c.xfer.mu.Unlock()

	// A fetch has no destination node -- the user's own computer collects it
	// -- so there is nothing to queue and it is ready as it stands.
	if t.ToNode == "(your computer)" {
		c.log.Info("fetch staged", "id", id, "bytes", n)
		return nil
	}

	// Otherwise ask the destination to take it. The limit is computed here
	// rather than on the node, because only the controller sees every machine.
	head, herr := c.Headroom(context.Background(), t.User)
	if herr != nil {
		head = -1
	}
	c.enqueue(t.ToNode, agentapi.Action{
		Kind: agentapi.ActionFileRecv, Transfer: id,
		FileUser: t.User, FilePath: t.ToPath, Limit: head,
	})
	c.log.Info("transfer staged", "id", id, "bytes", n, "to", t.ToNode)
	return nil
}

// OpenTransfer serves the staged blob to the destination node.
func (c *Controller) OpenTransfer(id int64, node string) (io.ReadCloser, int64, error) {
	t, err := c.transferFor(id, node, TransferStaged)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(c.blobPath(id))
	if err != nil {
		return nil, 0, err
	}
	return f, t.Size, nil
}

// transferFor checks that a node is a party to a transfer in the expected
// state.
//
// The node check is the authorisation: an agent may only touch a transfer it
// was actually asked to take part in, so a compromised node cannot read one
// user's files by guessing transfer ids.
func (c *Controller) transferFor(id int64, node string, want TransferState) (*Transfer, error) {
	c.xfer.mu.Lock()
	defer c.xfer.mu.Unlock()
	t := c.xfer.m[id]
	if t == nil {
		return nil, fmt.Errorf("no transfer %d", id)
	}
	party := t.FromNode
	if want == TransferStaged {
		party = t.ToNode
	}
	if node != party {
		return nil, fmt.Errorf("node %q is not a party to transfer %d", node, id)
	}
	if t.State != want {
		return nil, fmt.Errorf("transfer %d is %s, not %s", id, t.State, want)
	}
	return t, nil
}

// ApplyTransferResults records what agents reported.
func (c *Controller) ApplyTransferResults(ctx context.Context, node string,
	results []agentapi.TransferResult) {

	for _, r := range results {
		c.xfer.mu.Lock()
		t := c.xfer.m[r.Transfer]
		c.xfer.mu.Unlock()
		if t == nil {
			continue
		}
		switch {
		case !r.OK:
			c.failTransfer(r.Transfer, r.Error)
		case r.Kind == string(agentapi.ActionFileRecv):
			// The destination has it, so the listing can say so now rather
			// than at the receiving machine's next report -- twenty seconds
			// during which a copy that plainly worked was "not in the
			// index". Usage totals still come from what machines measure.
			if err := c.store.NoteUserFile(ctx, store.UserFile{
				Node: t.ToNode, User: t.User, Path: t.ToPath,
				Size: r.Bytes, MTime: c.now(),
			}); err != nil {
				c.log.Warn("index a transferred file", "id", t.ID, "err", err)
			} else {
				c.noted(t.ToNode, t.User, c.now())
			}
			// Delete the source only once the destination has it. The other
			// order would lose the file whenever the second hop failed.
			if t.Move {
				c.enqueue(t.FromNode, agentapi.Action{
					Kind: agentapi.ActionFileRemove, Transfer: t.ID,
					FileUser: t.User, FilePath: t.FromPath,
				})
				c.log.Info("transfer copied; removing the source", "id", t.ID)
				continue
			}
			c.finishTransfer(t.ID)
		case r.Kind == string(agentapi.ActionFileRemove):
			// The other half of a move: the original is gone, so it should
			// stop being listed on the machine that no longer has it.
			if err := c.store.ForgetUserFile(ctx, t.FromNode, t.User, t.FromPath); err != nil {
				c.log.Warn("drop a moved file from the index", "id", t.ID, "err", err)
			} else {
				c.noted(t.FromNode, t.User, c.now())
			}
			c.finishTransfer(t.ID)
		}
	}
}

func (c *Controller) finishTransfer(id int64) {
	c.xfer.mu.Lock()
	if t := c.xfer.m[id]; t != nil {
		t.State = TransferDone
	}
	c.xfer.mu.Unlock()
	os.Remove(c.blobPath(id))
	c.log.Info("transfer complete", "id", id)
}

func (c *Controller) failTransfer(id int64, msg string) {
	c.xfer.mu.Lock()
	if t := c.xfer.m[id]; t != nil && !t.Done() {
		t.State, t.Error = TransferFailed, msg
	}
	c.xfer.mu.Unlock()
	os.Remove(c.blobPath(id))
	c.log.Warn("transfer failed", "id", id, "err", msg)
}

// TransferStatus reports one transfer to whoever asked for it.
func (c *Controller) TransferStatus(id int64, user string) (*Transfer, error) {
	c.xfer.mu.Lock()
	defer c.xfer.mu.Unlock()
	t := c.xfer.m[id]
	if t == nil {
		return nil, fmt.Errorf("no transfer %d; it may have finished and been forgotten", id)
	}
	// One user must not be able to watch another's file movements.
	if t.User != user {
		return nil, fmt.Errorf("no transfer %d", id)
	}
	copy := *t
	return &copy, nil
}

// expireTransfer fails a transfer that never completes, so a caller waiting
// on it gets an answer rather than hanging on a node that will not return.
func (c *Controller) expireTransfer(id int64) {
	select {
	case <-time.After(TransferTTL):
	case <-c.stopCh:
		return
	}
	c.xfer.mu.Lock()
	t := c.xfer.m[id]
	stale := t != nil && !t.Done()
	c.xfer.mu.Unlock()
	if stale {
		c.failTransfer(id, fmt.Sprintf("timed out after %s; the machine may be offline", TransferTTL))
	}
	c.xfer.mu.Lock()
	delete(c.xfer.m, id)
	c.xfer.mu.Unlock()
}

// applyStorageReport records what a node says each account occupies.
//
// The report is that machine's complete view for the users it lists, so it
// replaces rather than merges: a file no longer listed has gone, and merging
// would leave deleted files counting against a limit forever.
func (c *Controller) applyStorageReport(ctx context.Context, hb agentapi.Heartbeat) {
	if !hb.StorageOK {
		return // this machine has not managed to look; keep what we had
	}
	now := c.now()
	// A report cannot speak for a file the controller learned about after
	// the walk behind it began. Applying it anyway deleted the index entry
	// for a file that had just been transferred -- on a home directory big
	// enough for the walk to take a second, which any Python environment
	// makes it. The next report, measured after the transfer, is
	// authoritative and puts everything back in one piece.
	measuredAt := time.UnixMilli(hb.StorageAt)
	// A complete report is authoritative for the machine, so an account it
	// does not mention has nothing here any more. Updating only the accounts
	// named would leave a deleted account's last files counted forever.
	keep := make([]string, 0, len(hb.Storage))
	for _, rep := range hb.Storage {
		keep = append(keep, rep.User)
	}
	if err := c.store.PruneNodeUsers(ctx, hb.Node, keep); err != nil {
		c.log.Warn("could not prune a node's storage index", "node", hb.Node, "err", err)
	}
	for _, rep := range hb.Storage {
		if c.notedSince(hb.Node, rep.User, measuredAt) {
			// Only the listing is held back. The usage figures are this
			// machine's own measurement and are what a limit is enforced
			// against, so they are recorded either way.
			usage := store.NodeUsage{
				Node: hb.Node, User: rep.User, Bytes: rep.Bytes,
				Files: rep.Files, Truncated: rep.Truncated,
			}
			if err := c.store.RecordNodeUsage(ctx, usage, now); err != nil {
				c.log.Warn("could not record a node's usage",
					"node", hb.Node, "user", rep.User, "err", err)
			}
			continue
		}
		files := make([]store.UserFile, 0, len(rep.Entries))
		for _, e := range rep.Entries {
			files = append(files, store.UserFile{
				Path: e.Path, Size: e.Size, MTime: time.UnixMilli(e.MTime),
			})
		}
		usage := store.NodeUsage{
			Node: hb.Node, User: rep.User, Bytes: rep.Bytes,
			Files: rep.Files, Truncated: rep.Truncated,
		}
		if err := c.store.ReplaceNodeFiles(ctx, hb.Node, rep.User, files, usage, now); err != nil {
			c.log.Warn("could not index a node's user files",
				"node", hb.Node, "user", rep.User, "err", err)
		}
	}
}

// noteKey identifies one account's files on one machine.
func noteKey(node, user string) string { return node + "\x00" + user }

// noted records that the controller has learned of a file on a machine
// directly, from a transfer it arranged, rather than from that machine's
// report.
func (c *Controller) noted(node, user string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noteAt[noteKey(node, user)] = at
}

// notedSince reports whether anything was learned about this account's files
// on this machine after the given time -- in which case a report measured
// then is too old to replace the listing.
func (c *Controller) notedSince(node, user string, measuredAt time.Time) bool {
	if measuredAt.IsZero() {
		return false // an agent that does not say; trust its report as before
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.noteAt[noteKey(node, user)]
	return ok && at.After(measuredAt)
}

// StageUpload accepts a file from a user's own computer and sends it to a
// machine in the cluster.
//
// The client is the source here rather than a node, so there is only one hop
// to arrange: stage the bytes, then ask the destination to take them.
func (c *Controller) StageUpload(ctx context.Context, user, toNode, toPath string,
	r io.Reader) (*Transfer, error) {

	toNode, err := c.NodeIdentity(ctx, toNode)
	if err != nil {
		return nil, err
	}
	head, err := c.Headroom(ctx, user)
	if err != nil {
		return nil, err
	}

	c.xfer.mu.Lock()
	c.xfer.next++
	t := &Transfer{
		ID: c.xfer.next, User: user, FromNode: "(your computer)",
		ToNode: toNode, ToPath: toPath, State: TransferSending, At: c.now(),
	}
	c.xfer.m[t.ID] = t
	c.xfer.mu.Unlock()
	go c.expireTransfer(t.ID)

	if err := os.MkdirAll(filepath.Dir(c.blobPath(t.ID)), 0o700); err != nil {
		return nil, err
	}
	f, err := os.Create(c.blobPath(t.ID))
	if err != nil {
		return nil, err
	}
	// Bounded by the account's remaining allowance, checked as the copy
	// proceeds. A declared length could be a lie, and a stream has none.
	var n int64
	if head < 0 {
		n, err = io.Copy(f, io.LimitReader(r, maxUserFileBytes))
	} else {
		n, err = io.Copy(f, io.LimitReader(r, head+1))
		if err == nil && n > head {
			f.Close()
			os.Remove(c.blobPath(t.ID))
			c.failTransfer(t.ID, "over the storage limit")
			return nil, fmt.Errorf(
				"this file would put you over your cluster-wide storage limit.\n"+
					"You have %s left; see where the rest has gone with 'shome fs du'", mib(head))
		}
	}
	cerr := f.Close()
	if err != nil || cerr != nil {
		os.Remove(c.blobPath(t.ID))
		c.failTransfer(t.ID, "upload failed")
		if err == nil {
			err = cerr
		}
		return nil, err
	}

	c.xfer.mu.Lock()
	t.State, t.Size = TransferStaged, n
	c.xfer.mu.Unlock()
	c.enqueue(toNode, agentapi.Action{
		Kind: agentapi.ActionFileRecv, Transfer: t.ID,
		FileUser: user, FilePath: toPath, Limit: head,
	})
	c.log.Info("upload staged", "id", t.ID, "user", user, "to", toNode+":"+toPath, "bytes", n)
	return t, nil
}

// StartFetch asks a machine to send one of a user's files to the controller,
// so the user's own computer can then download it.
func (c *Controller) StartFetch(ctx context.Context, user, fromNode, fromPath string) (*Transfer, error) {
	fromNode, err := c.NodeIdentity(ctx, fromNode)
	if err != nil {
		return nil, err
	}
	src, err := c.store.FindUserFile(ctx, user, fromNode, fromPath)
	if err != nil {
		return nil, err
	}
	c.xfer.mu.Lock()
	c.xfer.next++
	t := &Transfer{
		ID: c.xfer.next, User: user, FromNode: fromNode, FromPath: fromPath,
		ToNode: "(your computer)", Size: src.Size, State: TransferSending, At: c.now(),
	}
	c.xfer.m[t.ID] = t
	c.xfer.mu.Unlock()
	go c.expireTransfer(t.ID)

	if err := os.MkdirAll(filepath.Dir(c.blobPath(t.ID)), 0o700); err != nil {
		return nil, err
	}
	c.enqueue(fromNode, agentapi.Action{
		Kind: agentapi.ActionFileSend, Transfer: t.ID,
		FileUser: user, FilePath: fromPath,
	})
	return t, nil
}

// OpenFetched serves a fetched file to the user who asked for it, then
// forgets it.
func (c *Controller) OpenFetched(id int64, user string) (io.ReadCloser, int64, error) {
	c.xfer.mu.Lock()
	t := c.xfer.m[id]
	c.xfer.mu.Unlock()
	if t == nil || t.User != user {
		return nil, 0, fmt.Errorf("no transfer %d", id)
	}
	if t.State != TransferStaged {
		return nil, 0, fmt.Errorf("transfer %d is %s, not ready", id, t.State)
	}
	f, err := os.Open(c.blobPath(id))
	if err != nil {
		return nil, 0, err
	}
	return f, t.Size, nil
}

// FinishFetch marks a download complete and removes the relayed copy.
func (c *Controller) FinishFetch(id int64) { c.finishTransfer(id) }
