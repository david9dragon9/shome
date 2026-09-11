//go:build darwin

package ctl_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/ctl"
)

// End-to-end file movement, over the same mTLS path a remote machine uses.
//
// These tests are the ones that matter for `shome fs`: the unit tests check
// the bookkeeping, but only a real agent walking a real directory and
// heartbeating to a real controller proves the feature works at all.

// upload streams a local file into a machine's storage as the given account.
func (h *harness) upload(token, node, path string, body []byte) (ctl.Transfer, int) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://shome/fs/upload?node=%s&path=%s",
		url.QueryEscape(node), url.QueryEscape(path)), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.cli.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var t ctl.Transfer
	json.NewDecoder(resp.Body).Decode(&t)
	return t, resp.StatusCode
}

// awaitTransfer waits for a transfer to reach a terminal state.
func (h *harness) awaitTransfer(token string, id int64) ctl.Transfer {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last ctl.Transfer
	for time.Now().Before(deadline) {
		var t ctl.Transfer
		if code := h.doAs(token, "GET", fmt.Sprintf("/fs/transfer/%d", id), nil, &t); code != 200 {
			h.t.Fatalf("transfer %d status: %d", id, code)
		}
		last = t
		if t.State == ctl.TransferDone || t.State == ctl.TransferFailed {
			return t
		}
		time.Sleep(30 * time.Millisecond)
	}
	h.t.Fatalf("transfer %d never finished; last state %s", id, last.State)
	return last
}

// awaitIndexed waits for the controller's index to show an expected total,
// which happens on the heartbeat after the agent's walk.
func (h *harness) awaitIndexed(token string, wantBytes int64) ctl.FSUsage {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last ctl.FSUsage
	for time.Now().Before(deadline) {
		var u ctl.FSUsage
		if code := h.doAs(token, "GET", "/fs/usage", nil, &u); code != 200 {
			h.t.Fatalf("GET /fs/usage: %d", code)
		}
		last = u
		if u.TotalBytes == wantBytes {
			return u
		}
		time.Sleep(30 * time.Millisecond)
	}
	h.t.Fatalf("index never reached %d bytes; last total %d (%+v)",
		wantBytes, last.TotalBytes, last.Nodes)
	return last
}

func (h *harness) list(token, node, path string) []ctl.FSEntry {
	h.t.Helper()
	var es []ctl.FSEntry
	q := "/fs?node=" + url.QueryEscape(node) + "&path=" + url.QueryEscape(path)
	if code := h.doAs(token, "GET", q, nil, &es); code != 200 {
		h.t.Fatalf("GET %s: %d", q, code)
	}
	return es
}

// The whole journey: a file from the user's computer, onto one machine, then
// copied to a second, and accounted for on both.
func TestFileTravelsFromLaptopToOneMachineThenAnother(t *testing.T) {
	h := startN(t, 2)
	if len(h.nodes) < 2 {
		t.Skip("needs two nodes")
	}
	tok := h.addUser("alice")
	body := []byte(strings.Repeat("payload", 1000)) // 7000 bytes
	from, to := h.nodes[0], h.nodes[1]

	tr, code := h.upload(tok, from, "data/model.bin", body)
	if code != 200 {
		t.Fatalf("upload: status %d", code)
	}
	if got := h.awaitTransfer(tok, tr.ID); got.State != ctl.TransferDone {
		t.Fatalf("upload ended %s: %s", got.State, got.Error)
	}

	// The machine actually has the bytes, at the account's own path.
	onDisk := filepath.Join(h.root, from, "users", "alice", "data", "model.bin")
	got, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("the file is not on %s: %v", from, err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("%s holds %d bytes, want %d", from, len(got), len(body))
	}

	// And the controller's index picks it up from the agent's own walk.
	u := h.awaitIndexed(tok, int64(len(body)))
	if len(u.Nodes) != 1 || u.Nodes[0].Node != from {
		t.Fatalf("usage = %+v, want only %s", u.Nodes, from)
	}
	if es := h.list(tok, from, ""); len(es) != 1 || es[0].Path != "data/model.bin" {
		t.Fatalf("listing = %+v, want data/model.bin", es)
	}

	// Copy it to the second machine, controller-relayed in two hops.
	var cp ctl.Transfer
	if code := h.doAs(tok, "POST", "/fs/transfer", map[string]any{
		"from_node": from, "from_path": "data/model.bin",
		"to_node": to, "to_path": "model.bin",
	}, &cp); code != 200 {
		t.Fatalf("copy: status %d", code)
	}
	if got := h.awaitTransfer(tok, cp.ID); got.State != ctl.TransferDone {
		t.Fatalf("copy ended %s: %s", got.State, got.Error)
	}

	second, err := os.ReadFile(filepath.Join(h.root, to, "users", "alice", "model.bin"))
	if err != nil {
		t.Fatalf("the copy is not on %s: %v", to, err)
	}
	if !bytes.Equal(second, body) {
		t.Fatal("the copy does not match the original")
	}

	// The point of the whole design: a copy costs its size again, and the
	// total spans machines.
	u = h.awaitIndexed(tok, int64(2*len(body)))
	if len(u.Nodes) != 2 {
		t.Fatalf("usage = %+v, want both machines", u.Nodes)
	}
	// The original is still there: this was a copy, not a move.
	if _, err := os.Stat(onDisk); err != nil {
		t.Errorf("a copy removed the original: %v", err)
	}
}

// A move must end with exactly one copy, and the original must survive until
// the destination has it.
func TestMoveLeavesExactlyOneCopy(t *testing.T) {
	h := startN(t, 2)
	if len(h.nodes) < 2 {
		t.Skip("needs two nodes")
	}
	tok := h.addUser("alice")
	body := []byte(strings.Repeat("x", 4096))
	from, to := h.nodes[0], h.nodes[1]

	tr, _ := h.upload(tok, from, "f.bin", body)
	h.awaitTransfer(tok, tr.ID)
	h.awaitIndexed(tok, int64(len(body)))

	var mv ctl.Transfer
	if code := h.doAs(tok, "POST", "/fs/transfer", map[string]any{
		"from_node": from, "from_path": "f.bin",
		"to_node": to, "to_path": "f.bin", "move": true,
	}, &mv); code != 200 {
		t.Fatalf("move: status %d", code)
	}
	if got := h.awaitTransfer(tok, mv.ID); got.State != ctl.TransferDone {
		t.Fatalf("move ended %s: %s", got.State, got.Error)
	}

	if _, err := os.Stat(filepath.Join(h.root, to, "users", "alice", "f.bin")); err != nil {
		t.Fatalf("the destination does not have the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.root, from, "users", "alice", "f.bin")); !os.IsNotExist(err) {
		t.Errorf("the original is still on %s: %v", from, err)
	}
	// One copy, so the total is back to one file's worth.
	h.awaitIndexed(tok, int64(len(body)))
}

// Deleting on one machine frees exactly that machine's space.
func TestRemoveFreesTheAccountsAllowance(t *testing.T) {
	h := startN(t, 2)
	if len(h.nodes) < 2 {
		t.Skip("needs two nodes")
	}
	tok := h.addUser("alice")
	body := []byte(strings.Repeat("y", 2048))

	for _, n := range h.nodes[:2] {
		tr, _ := h.upload(tok, n, "f.bin", body)
		h.awaitTransfer(tok, tr.ID)
	}
	h.awaitIndexed(tok, int64(2*len(body)))

	if code := h.doAs(tok, "POST", "/fs/remove", map[string]any{
		"node": h.nodes[0], "path": "f.bin",
	}, nil); code != 200 {
		t.Fatalf("rm: status %d", code)
	}
	// Back to one copy's worth, from the agent's own re-measurement.
	h.awaitIndexed(tok, int64(len(body)))
}

// The cluster-wide limit is enforced against what machines report, not
// against a running tally that could drift.
func TestClusterWideLimitStopsTheSecondCopy(t *testing.T) {
	h := startN(t, 2)
	if len(h.nodes) < 2 {
		t.Skip("needs two nodes")
	}
	tok := h.addUser("alice")
	// 1 MiB allowance; a 700 KiB file fits once and not twice.
	oneMiB := int64(1)
	if code := h.do("POST", "/users/alice/qos", map[string]any{"max_disk_mb": &oneMiB}, nil); code != 200 {
		t.Fatalf("set the limit: status %d", code)
	}
	body := []byte(strings.Repeat("z", 700<<10))
	from, to := h.nodes[0], h.nodes[1]

	tr, code := h.upload(tok, from, "f.bin", body)
	if code != 200 {
		t.Fatalf("first upload: status %d", code)
	}
	h.awaitTransfer(tok, tr.ID)
	h.awaitIndexed(tok, int64(len(body)))

	// A copy would need 700 KiB more against 324 KiB left.
	var cp ctl.Transfer
	code = h.doAs(tok, "POST", "/fs/transfer", map[string]any{
		"from_node": from, "from_path": "f.bin",
		"to_node": to, "to_path": "f.bin",
	}, &cp)
	if code == 200 {
		t.Fatal("the copy was accepted although it exceeds the cluster-wide limit")
	}
	if _, err := os.Stat(filepath.Join(h.root, to, "users", "alice", "f.bin")); !os.IsNotExist(err) {
		t.Errorf("a refused copy was written anyway: %v", err)
	}

	// A second upload from the laptop is refused by the same limit -- the
	// other way in must not be a way around it.
	if _, code := h.upload(tok, to, "other.bin", body); code == 200 {
		t.Error("an upload over the cluster-wide limit was accepted")
	}
}

// Files are indexed to an account, so one user must not see or touch
// another's, on any machine.
func TestAccountsCannotSeeEachOthersFiles(t *testing.T) {
	h := startN(t, 1)
	alice, bob := h.addUser("alice"), h.addUser("bob")
	node := h.nodes[0]

	tr, _ := h.upload(alice, node, "secret.bin", []byte("alice-only"))
	h.awaitTransfer(alice, tr.ID)
	h.awaitIndexed(alice, 10)

	if es := h.list(bob, node, ""); len(es) != 0 {
		t.Errorf("bob sees alice's files: %+v", es)
	}
	var u ctl.FSUsage
	h.doAs(bob, "GET", "/fs/usage", nil, &u)
	if u.TotalBytes != 0 {
		t.Errorf("bob's usage = %d, want 0", u.TotalBytes)
	}
	// Same path, different account: two separate files, and bob's removal
	// must not touch alice's.
	tr2, _ := h.upload(bob, node, "secret.bin", []byte("bob"))
	h.awaitTransfer(bob, tr2.ID)
	if code := h.doAs(bob, "POST", "/fs/remove", map[string]any{
		"node": node, "path": "secret.bin",
	}, nil); code != 200 {
		t.Fatalf("bob rm: status %d", code)
	}
	h.awaitIndexed(bob, 0)

	got, err := os.ReadFile(filepath.Join(h.root, node, "users", "alice", "secret.bin"))
	if err != nil || string(got) != "alice-only" {
		t.Fatalf("bob's deletion affected alice's file: %q %v", got, err)
	}
}

// A path must not be able to escape the account's own directory, all the way
// through the API and the agent.
func TestUploadCannotEscapeTheAccountsDirectory(t *testing.T) {
	h := startN(t, 1)
	tok := h.addUser("alice")
	node := h.nodes[0]

	for _, bad := range []string{"../bob/planted", "../../planted", "a/../../planted"} {
		tr, code := h.upload(tok, node, bad, []byte("planted"))
		if code == 200 && tr.ID != 0 {
			// Staging may accept it; the machine must still refuse to write.
			if got := h.awaitTransfer(tok, tr.ID); got.State == ctl.TransferDone {
				t.Errorf("upload to %q succeeded", bad)
			}
		}
	}
	// Nothing landed outside alice's directory.
	root := filepath.Join(h.root, node, "users")
	err := filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if err == nil && strings.Contains(filepath.Base(p), "planted") &&
			!strings.Contains(p, filepath.Join("users", "alice")) {
			t.Errorf("a file escaped to %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Downloading is the reverse two hops: the machine sends to the controller,
// the user collects.
func TestFetchBringsAFileBack(t *testing.T) {
	h := startN(t, 1)
	tok := h.addUser("alice")
	node := h.nodes[0]
	body := []byte(strings.Repeat("result,", 500))

	tr, _ := h.upload(tok, node, "out.csv", body)
	h.awaitTransfer(tok, tr.ID)
	h.awaitIndexed(tok, int64(len(body)))

	var f ctl.Transfer
	if code := h.doAs(tok, "POST", "/fs/fetch", map[string]any{
		"node": node, "path": "out.csv",
	}, &f); code != 200 {
		t.Fatalf("fetch: status %d", code)
	}

	// Wait for the machine to hand it over, then stream it down.
	deadline := time.Now().Add(20 * time.Second)
	var got []byte
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://shome/fs/fetch/%d", f.ID), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := h.cli.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode == 200 {
			got, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			break
		}
		resp.Body.Close()
		time.Sleep(30 * time.Millisecond)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(body))
	}
	// The relayed copy is not the user's to be charged for, so the total is
	// still one file.
	h.awaitIndexed(tok, int64(len(body)))
}

// A machine that leaves stops costing its users anything.
func TestForgettingAMachineFreesItsUsers(t *testing.T) {
	h := startN(t, 2)
	if len(h.nodes) < 2 {
		t.Skip("needs two nodes")
	}
	tok := h.addUser("alice")
	body := []byte(strings.Repeat("q", 1024))
	for _, n := range h.nodes[:2] {
		tr, _ := h.upload(tok, n, "f.bin", body)
		h.awaitTransfer(tok, tr.ID)
	}
	h.awaitIndexed(tok, int64(2*len(body)))

	if code := h.do("POST", "/node/"+h.nodes[1]+"/remove", nil, nil); code != 200 {
		t.Fatalf("forget node: status %d", code)
	}
	// The departed machine's files stop counting. The remaining agent keeps
	// reporting its own, so the total settles at one file's worth.
	h.awaitIndexed(tok, int64(len(body)))
}
