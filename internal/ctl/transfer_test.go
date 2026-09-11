package ctl

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/qos"
	"github.com/davidwu/shome/internal/store"
	"github.com/davidwu/shome/internal/userfs"
)

func fsFixture(t *testing.T, nodes ...string) (*Controller, *store.Store, context.Context) {
	t.Helper()
	c, st := qosFixture(t)
	ctx := context.Background()
	for _, n := range nodes {
		if err := st.UpsertNode(ctx, n, "127.0.0.1:1", platform.Capabilities{
			OS: "test", Arch: "test", CPUs: 1, MemBytes: 1 << 30,
		}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return c, st, ctx
}

// have records that a machine holds a file for alice.
func have(t *testing.T, st *store.Store, node string, files map[string]int64) {
	t.Helper()
	var list []store.UserFile
	var u store.NodeUsage
	for p, n := range files {
		list = append(list, store.UserFile{Path: p, Size: n, MTime: time.Now()})
		u.Bytes += n
		u.Files++
	}
	if err := st.ReplaceNodeFiles(context.Background(), node, "alice", list, u, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func diskMB(t *testing.T, c *Controller, mb int64) {
	t.Helper()
	if err := c.SetUserQoS(context.Background(), "alice", qos.Limits{MaxDiskMB: &mb}); err != nil {
		t.Fatal(err)
	}
}

// queued returns the actions waiting for a node, and clears them, standing in
// for the heartbeat that would collect them.
func queued(c *Controller, node string) []agentapi.Action {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.pending[node]
	c.pending[node] = nil
	return a
}

// The headline requirement: one limit, spanning every machine. A file that
// fits on the destination must still be refused if the account's total across
// the cluster would exceed its allowance.
func TestCopyRefusedByTheCrossMachineTotal(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	diskMB(t, c, 10)
	have(t, st, "mini", map[string]int64{"model.bin": 6 << 20})

	// 6 MiB on mini, 6 more on gpu, against a 10 MiB cluster-wide limit.
	_, err := c.StartTransfer(ctx, "alice", "mini", "model.bin", "gpu", "model.bin", false)
	if err == nil {
		t.Fatal("copy allowed; the cluster-wide total would be 12 MiB against a 10 MiB limit")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error does not mention the limit: %v", err)
	}
	if len(queued(c, "mini")) != 0 {
		t.Error("a refused transfer still queued work for the source machine")
	}

	// The same copy is fine once there is room.
	diskMB(t, c, 20)
	if _, err := c.StartTransfer(ctx, "alice", "mini", "model.bin", "gpu", "model.bin", false); err != nil {
		t.Fatalf("copy within the limit refused: %v", err)
	}
}

// A move within one machine renames rather than duplicating, so it must not
// be charged as if it were a second copy.
func TestMoveOnOneMachineIsNotChargedTwice(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini")
	diskMB(t, c, 10)
	have(t, st, "mini", map[string]int64{"a.bin": 9 << 20})

	if _, err := c.StartTransfer(ctx, "alice", "mini", "a.bin", "mini", "b.bin", true); err != nil {
		t.Fatalf("rename within one machine refused: %v", err)
	}
}

// Overwriting a file reclaims what the old one occupied, so the charge is the
// difference, not the whole new size.
func TestOverwriteIsChargedTheDifference(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	diskMB(t, c, 20)
	have(t, st, "mini", map[string]int64{"f": 9 << 20})
	have(t, st, "gpu", map[string]int64{"f": 9 << 20})

	// 18 MiB used of 20. Replacing gpu:f with mini:f is 9 in and 9 out, so it
	// fits; charging the full 9 without the credit would refuse it.
	if _, err := c.StartTransfer(ctx, "alice", "mini", "f", "gpu", "f", false); err != nil {
		t.Fatalf("overwrite refused despite reclaiming the same space: %v", err)
	}
}

func TestUnlimitedDiskAllowsAnything(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	diskMB(t, c, qos.Unlimited)
	have(t, st, "mini", map[string]int64{"huge": 1 << 40})

	if _, err := c.StartTransfer(ctx, "alice", "mini", "huge", "gpu", "huge", false); err != nil {
		t.Fatalf("unlimited disk refused a transfer: %v", err)
	}
}

func TestTransferRejectsUnknownMachinesAndFiles(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini")
	have(t, st, "mini", map[string]int64{"f": 1})

	if _, err := c.StartTransfer(ctx, "alice", "mini", "f", "nowhere", "f", false); err == nil {
		t.Error("transfer to an unknown machine was accepted")
	}
	if _, err := c.StartTransfer(ctx, "alice", "nowhere", "f", "mini", "f", false); err == nil {
		t.Error("transfer from an unknown machine was accepted")
	}
	if _, err := c.StartTransfer(ctx, "alice", "mini", "absent", "mini", "f", false); err == nil {
		t.Error("transfer of a file not in the index was accepted")
	}
	if _, err := c.StartTransfer(ctx, "alice", "mini", "f", "mini", "f", false); err == nil {
		t.Error("transfer of a file onto itself was accepted")
	}
}

// The order that matters: a move deletes the original only after the
// destination confirms it has the file. The other order loses data whenever
// the second hop fails.
func TestMoveDeletesTheSourceOnlyAfterTheDestinationConfirms(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	have(t, st, "mini", map[string]int64{"f": 100})

	tr, err := c.StartTransfer(ctx, "alice", "mini", "f", "gpu", "f", true)
	if err != nil {
		t.Fatal(err)
	}

	// The source has been asked to send, and nothing has been deleted.
	acts := queued(c, "mini")
	if len(acts) != 1 || acts[0].Kind != agentapi.ActionFileSend {
		t.Fatalf("first hop queued %+v, want one file-send", acts)
	}

	// The source reports it sent the file. Still no deletion: the destination
	// has not written it yet.
	c.ApplyTransferResults(ctx, "mini", []agentapi.TransferResult{
		{Transfer: tr.ID, Kind: string(agentapi.ActionFileSend), OK: true},
	})
	if got := queued(c, "mini"); len(got) != 0 {
		t.Fatalf("source was told to delete before the destination confirmed: %+v", got)
	}

	// Now the destination confirms, and only now is the original removed.
	c.ApplyTransferResults(ctx, "gpu", []agentapi.TransferResult{
		{Transfer: tr.ID, Kind: string(agentapi.ActionFileRecv), OK: true},
	})
	rm := queued(c, "mini")
	if len(rm) != 1 || rm[0].Kind != agentapi.ActionFileRemove || rm[0].FilePath != "f" {
		t.Fatalf("after confirmation the source was told %+v, want a file-remove of 'f'", rm)
	}

	c.ApplyTransferResults(ctx, "mini", []agentapi.TransferResult{
		{Transfer: tr.ID, Kind: string(agentapi.ActionFileRemove), OK: true},
	})
	if s, _ := c.TransferStatus(tr.ID, "alice"); s.State != TransferDone {
		t.Errorf("final state = %s, want %s", s.State, TransferDone)
	}
}

// A failed destination write must leave the original alone.
func TestFailedMoveKeepsTheOriginal(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	have(t, st, "mini", map[string]int64{"f": 100})

	tr, err := c.StartTransfer(ctx, "alice", "mini", "f", "gpu", "f", true)
	if err != nil {
		t.Fatal(err)
	}
	queued(c, "mini")
	c.ApplyTransferResults(ctx, "gpu", []agentapi.TransferResult{
		{Transfer: tr.ID, Kind: string(agentapi.ActionFileRecv), OK: false, Error: "disk full"},
	})
	if got := queued(c, "mini"); len(got) != 0 {
		t.Fatalf("a failed move still queued a deletion: %+v", got)
	}
	s, _ := c.TransferStatus(tr.ID, "alice")
	if s.State != TransferFailed || !strings.Contains(s.Error, "disk full") {
		t.Errorf("state = %s %q, want failed with the reason", s.State, s.Error)
	}
}

// One account must not be able to see or touch another's transfer, since the
// id is a small integer anyone could guess.
func TestTransferStatusIsScopedToItsOwner(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	have(t, st, "mini", map[string]int64{"f": 100})
	if _, err := st.CreateUser(ctx, "bob", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}

	tr, err := c.StartTransfer(ctx, "alice", "mini", "f", "gpu", "f", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.TransferStatus(tr.ID, "bob"); err == nil {
		t.Error("bob can read alice's transfer")
	}
	if _, err := c.TransferStatus(tr.ID, "alice"); err != nil {
		t.Errorf("alice cannot read her own transfer: %v", err)
	}
}

// A machine leaving must not leave its files counted against the account,
// which would put them permanently over a limit they cannot get under.
func TestDepartedMachineFreesTheAccountsAllowance(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	diskMB(t, c, 10)
	have(t, st, "mini", map[string]int64{"f": 4 << 20})
	have(t, st, "gpu", map[string]int64{"f": 4 << 20})

	head, err := c.Headroom(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if head != 2<<20 {
		t.Fatalf("headroom = %d, want %d", head, 2<<20)
	}

	if err := st.ForgetNodeStorage(ctx, "gpu"); err != nil {
		t.Fatal(err)
	}
	head, _ = c.Headroom(ctx, "alice")
	if head != 6<<20 {
		t.Errorf("headroom after gpu left = %d, want %d", head, 6<<20)
	}
}

// Zero means nothing may be stored, and must not be read as unlimited.
func TestZeroDiskAllowsNothing(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	diskMB(t, c, 0)
	have(t, st, "mini", map[string]int64{"f": 1})

	if _, err := c.StartTransfer(ctx, "alice", "mini", "f", "gpu", "f", false); err == nil {
		t.Error("a zero disk limit allowed a copy")
	}
	if head, _ := c.Headroom(ctx, "alice"); head != 0 {
		t.Errorf("headroom = %d, want 0", head)
	}
}

// Uploading from the user's own computer is bounded by the same cluster-wide
// allowance, checked as the bytes arrive rather than from a declared length a
// client could lie about.
func TestUploadIsBoundedByTheClusterWideAllowance(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini")
	diskMB(t, c, 1)
	have(t, st, "mini", map[string]int64{"used": 1<<20 - 100})

	body := strings.NewReader(strings.Repeat("x", 4096))
	if _, err := c.StageUpload(ctx, "alice", "mini", "new", body); err == nil {
		t.Fatal("upload allowed with only 100 bytes of allowance left")
	}

	// And within the allowance it goes through, staged for the destination.
	small := strings.NewReader(strings.Repeat("x", 50))
	tr, err := c.StageUpload(ctx, "alice", "mini", "small", small)
	if err != nil {
		t.Fatalf("upload within the allowance refused: %v", err)
	}
	if tr.State != TransferStaged || tr.Size != 50 {
		t.Errorf("staged upload = %s / %d bytes, want staged / 50", tr.State, tr.Size)
	}
	acts := queued(c, "mini")
	if len(acts) != 1 || acts[0].Kind != agentapi.ActionFileRecv || acts[0].FilePath != "small" {
		t.Fatalf("upload queued %+v, want one file-recv of 'small'", acts)
	}
	// The machine is told the remaining allowance, so it can refuse too --
	// only the controller can see every machine, so the node cannot work it
	// out for itself.
	if acts[0].Limit <= 0 {
		t.Errorf("file-recv carried limit %d, want the account's headroom", acts[0].Limit)
	}
}

func TestUploadRejectsUnknownMachines(t *testing.T) {
	c, _, ctx := fsFixture(t, "mini")
	if _, err := c.StageUpload(ctx, "alice", "nowhere", "f", strings.NewReader("x")); err == nil {
		t.Error("upload to an unknown machine was accepted")
	}
}

// Regression: `shome storage` and `shome fs` used to be two directories with
// two independent quota checks, so an account could hold its full allowance
// once in each. They must be the same files.
func TestStorageAndFsAreTheSameFiles(t *testing.T) {
	c, _ := dashFixture(t)
	got, err := c.Storage().Resolve("alice", "f")
	if err != nil {
		t.Fatal(err)
	}
	want, err := userfs.New(c.root).Resolve("alice", "f")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("shome storage writes to %q but shome fs reads %q; they must be one place", got, want)
	}
}

// A machine that has not managed to measure its files must not be read as
// having none, or an account's usage would vanish and reappear.
func TestIncompleteStorageReportIsIgnored(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini")
	have(t, st, "mini", map[string]int64{"f": 500})

	c.applyStorageReport(ctx, agentapi.Heartbeat{Node: "mini", StorageOK: false})
	if b, _, _ := mustCtlTotal(t, c, "alice"); b != 500 {
		t.Errorf("total after an incomplete report = %d, want 500 kept", b)
	}
}

// The other half: once a machine does report, an account it omits has nothing
// there. Otherwise deleting your last file on a machine would keep costing
// you for it forever.
func TestCompleteStorageReportClearsAccountsItOmits(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini")
	have(t, st, "mini", map[string]int64{"f": 500})
	if _, err := st.CreateUser(ctx, "bob", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}

	// mini now reports only bob: alice has removed everything she had there.
	c.applyStorageReport(ctx, agentapi.Heartbeat{
		Node: "mini", StorageOK: true,
		Storage: []agentapi.UserStorage{{User: "bob", Bytes: 7, Files: 1}},
	})
	if b, _, _ := mustCtlTotal(t, c, "alice"); b != 0 {
		t.Errorf("alice still charged %d for files she deleted", b)
	}
	if b, _, _ := mustCtlTotal(t, c, "bob"); b != 7 {
		t.Errorf("bob total = %d, want 7", b)
	}

	// An empty complete report clears the machine entirely.
	c.applyStorageReport(ctx, agentapi.Heartbeat{Node: "mini", StorageOK: true})
	if b, _, _ := mustCtlTotal(t, c, "bob"); b != 0 {
		t.Errorf("bob total after an empty report = %d, want 0", b)
	}
}

// One machine's report must not clear another's, since they arrive
// independently.
func TestStorageReportPruningIsPerMachine(t *testing.T) {
	c, st, ctx := fsFixture(t, "mini", "gpu")
	have(t, st, "mini", map[string]int64{"f": 100})
	have(t, st, "gpu", map[string]int64{"f": 200})

	c.applyStorageReport(ctx, agentapi.Heartbeat{Node: "mini", StorageOK: true})
	if b, _, _ := mustCtlTotal(t, c, "alice"); b != 200 {
		t.Errorf("total = %d, want 200 -- mini's report must not clear gpu", b)
	}
}

func mustCtlTotal(t *testing.T, c *Controller, user string) (int64, int, error) {
	t.Helper()
	b, f, err := c.store.UserDiskTotal(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	return b, f, nil
}
