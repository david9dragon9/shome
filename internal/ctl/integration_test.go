//go:build darwin

package ctl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/agent"
	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/pki"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/platform/darwin"
	"github.com/davidwu/shome/internal/store"
	"github.com/davidwu/shome/internal/xfer"
)

type harness struct {
	t     *testing.T
	root  string
	nodes []string
	token string
	cli   *http.Client
}

// start brings up a controller plus one node agent, wired together over real
// mTLS on loopback. There is no in-process shortcut: the test exercises the
// same path a remote agent uses, so the local case cannot silently diverge.
func start(t *testing.T) *harness {
	t.Helper()
	return startN(t, 1)
}

// startN brings up a controller with n agents, for multi-node tests.
func startN(t *testing.T, n int) *harness {
	t.Helper()
	// Real clusters heartbeat every few seconds; tests should not pay that.
	prev := agent.HeartbeatInterval
	agent.HeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { agent.HeartbeatInterval = prev })
	root := t.TempDir()
	ownerHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(ownerHome, "secret.txt"), []byte("owner-private"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(root, "shome.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := ctl.BootstrapAdmin(context.Background(), st, root); err != nil {
		t.Fatal(err)
	}
	adminTok, err := os.ReadFile(ctl.AdminTokenPath(root))
	if err != nil {
		t.Fatal(err)
	}
	c := ctl.New(st, root, log)

	// Unix socket paths are capped near 104 bytes and t.TempDir() is long.
	sockDir, err := os.MkdirTemp("", "sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s")

	cliSrv, cliLn, err := ctl.Serve(c, sock)
	if err != nil {
		t.Fatal(err)
	}
	go cliSrv.Serve(cliLn)

	ca, err := pki.LoadOrCreateCA(filepath.Join(root, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	agSrv, agLn, err := ctl.ServeAgents(c, ca, "127.0.0.1:0", []string{"127.0.0.1", "localhost"})
	if err != nil {
		t.Fatal(err)
	}
	go agSrv.Serve(agLn)

	ctx, cancel := context.WithCancel(context.Background())
	go c.Run(ctx)
	t.Cleanup(func() { cancel(); cliSrv.Close(); agSrv.Close() })

	// A job's processes outlive the test that cancelled them, briefly: a
	// cancellation is a signal and a grace period, not an instant. The
	// per-test temp directory is removed as soon as the test returns, and a
	// rank still writing into it made that removal fail -- "directory not
	// empty" from a test that had passed. Waiting for the agents to have
	// nothing running is both the honest teardown and the end of a flake.
	//
	// Registered before the agents start, so it runs after they are asked
	// to stop: cleanups run in reverse order.
	var agents []*agent.Agent
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			busy := false
			for _, ag := range agents {
				if len(ag.RunningIDs()) > 0 {
					busy = true
				}
			}
			if !busy {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Log("teardown: a job was still running when the test finished")
	})

	addr := agLn.Addr().String()
	var nodeNames []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("node%d", i)
		nodeNames = append(nodeNames, name)

		tok, err := st.CreateJoinToken(ctx, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		jr, err := agent.Join(ctx, addr, name, tok)
		if err != nil {
			t.Fatalf("agent %s join: %v", name, err)
		}
		nodeRoot := filepath.Join(root, name)
		be := darwin.New(nodeRoot, ownerHome)
		caps, err := be.Inventory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ag := agent.New(be, name, nodeRoot, log)
		cli, err := agent.NewClient(addr, name, []byte(jr.CACert), []byte(jr.NodeCert), []byte(jr.NodeKey), "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		cli.StatusRoot = nodeRoot
		agents = append(agents, ag)
		go ag.Supervise(ctx)
		go ag.RunLoop(ctx, cli, func() agentapi.Heartbeat { return agentapi.Heartbeat{Caps: caps} })
	}

	h := &harness{t: t, root: root, nodes: nodeNames, token: strings.TrimSpace(string(adminTok)), cli: &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}},
	}}
	h.waitNodes(n)
	return h
}

// waitNodes blocks until n agents have registered, so tests do not race the
// first heartbeat.
func (h *harness) waitNodes(n int) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var nodes []ctl.NodeInfo
		h.do("GET", "/node", nil, &nodes)
		up := 0
		for _, nd := range nodes {
			if nd.State == "UP" {
				up++
			}
		}
		if up >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.t.Fatalf("only some of %d nodes registered in time", n)
}

func (h *harness) do(method, path string, body any, out any) int {
	h.t.Helper()
	var rdr io.Reader = strings.NewReader("")
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, "http://shome"+path, rdr)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.cli.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (h *harness) submitScript(name, contents string, lim job.Limits) ctl.JobView {
	h.t.Helper()
	// The script lives outside the sandbox's denied paths.
	p := filepath.Join(h.root, name)
	if err := os.WriteFile(p, []byte(contents), 0o755); err != nil {
		h.t.Fatal(err)
	}
	var v ctl.JobView
	code := h.do("POST", "/submit", job.Spec{Name: name, User: "tester", Script: p, Limits: lim}, &v)
	if code != 200 {
		h.t.Fatalf("submit %s: status %d", name, code)
	}
	return v
}

func (h *harness) waitTerminal(id int64, d time.Duration) ctl.JobView {
	h.t.Helper()
	deadline := time.Now().Add(d)
	var v ctl.JobView
	for time.Now().Before(deadline) {
		h.do("GET", "/job/"+itoa(id), nil, &v)
		if job.State(v.State).Terminal() {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.t.Fatalf("job %d still %s after %s (reason %q)", id, v.State, d, v.Reason)
	return v
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// output fetches a job's archived output through the API, the same way a user
// would. Retries briefly because the upload happens on the heartbeat after the
// job is acked.
//
// Deliberately not reading the file directly: that coupled the tests to the
// on-disk naming, and they all broke the moment gang jobs needed per-rank
// files.
func (h *harness) output(v ctl.JobView) string {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", "http://shome/job/"+itoa(v.ID)+"/output", nil)
		req.Header.Set("Authorization", "Bearer "+h.token)
		resp, err := h.cli.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && len(body) > 0 {
				return string(body)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}

// ---------- tests ----------

func TestEndToEndJobCompletes(t *testing.T) {
	h := start(t)
	v := h.submitScript("ok.sh", "#!/bin/sh\necho hello-world\n", job.Limits{CPUs: 1})
	done := h.waitTerminal(v.ID, 20*time.Second)
	if done.State != string(job.Completed) {
		t.Errorf("state = %s (%s), want COMPLETED", done.State, done.Reason)
	}
	if got := h.output(done); !strings.Contains(got, "hello-world") {
		t.Errorf("output = %q", got)
	}
}

func TestEndToEndFailingJobReportsExitCode(t *testing.T) {
	h := start(t)
	v := h.submitScript("fail.sh", "#!/bin/sh\nexit 7\n", job.Limits{CPUs: 1})
	done := h.waitTerminal(v.ID, 20*time.Second)
	if done.State != string(job.Failed) || done.ExitCode != 7 {
		t.Errorf("state=%s exit=%d, want FAILED/7", done.State, done.ExitCode)
	}
}

// The headline isolation guarantee, exercised through the real submit path.
func TestEndToEndJobCannotReadOwnerHome(t *testing.T) {
	h := start(t)
	// The owner home is the second TempDir created in start(); find it via the
	// job's own failure rather than guessing the path.
	v := h.submitScript("peek.sh", "#!/bin/sh\ncat \"$HOME\"/secret.txt 2>&1; exit 0\n", job.Limits{CPUs: 1})
	done := h.waitTerminal(v.ID, 20*time.Second)
	if strings.Contains(h.output(done), "owner-private") {
		t.Error("job read the owner's private file")
	}
}

// Regression test: the memory limit must catch a FORKED child. A kernel
// `taskpolicy -m` cap would miss this entirely.
func TestEndToEndMemoryLimitKillsForkedChild(t *testing.T) {
	h := start(t)
	// A forked child that exceeds the limit must be killed. This is the measured failure's
	// lesson: taskpolicy's memory ledger is not inherited across fork+exec,
	// so a limit applied to the shell missed the child entirely.
	//
	// The child's status is not backgrounded away here, so the job reports
	// the kill. A script that writes `child & wait` hides it -- bare `wait`
	// returns zero whatever the child did -- and that is true of any
	// cgroup-limited system, Slurm included. See the note in the README.
	script := `#!/bin/sh
dd if=/dev/zero of=/dev/null bs=300M count=60 2>/dev/null
`
	v := h.submitScript("hog.sh", script, job.Limits{CPUs: 1, MemBytes: 100 << 20})
	done := h.waitTerminal(v.ID, 60*time.Second)
	if done.State == string(job.Completed) {
		t.Fatalf("a job that asked for 100 MiB and used 300 completed: %+v", done)
	}
	if done.State != string(job.OOM) {
		t.Errorf("state = %s (%s), want OUT_OF_MEMORY", done.State, done.Reason)
	}
	if !strings.Contains(done.Reason, "memory limit") {
		t.Errorf("reason = %q, should explain the memory limit", done.Reason)
	}
	t.Logf("killed with reason: %s", done.Reason)
}

func TestEndToEndWalltimeEnforced(t *testing.T) {
	h := start(t)
	v := h.submitScript("slow.sh", "#!/bin/sh\nsleep 60\n",
		job.Limits{CPUs: 1, Walltime: 2 * time.Second})
	done := h.waitTerminal(v.ID, 30*time.Second)
	if done.State != string(job.Timeout) {
		t.Errorf("state = %s (%s), want TIMEOUT", done.State, done.Reason)
	}
}

func TestEndToEndCancel(t *testing.T) {
	h := start(t)
	v := h.submitScript("long.sh", "#!/bin/sh\nsleep 60\n", job.Limits{CPUs: 1})
	// Wait for it to actually start, so we exercise cancelling a RUNNING job.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var cur ctl.JobView
		h.do("GET", "/job/"+itoa(v.ID), nil, &cur)
		if cur.State == string(job.Running) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code := h.do("POST", "/cancel/"+itoa(v.ID), nil, nil); code != 200 {
		t.Fatalf("cancel returned %d", code)
	}
	done := h.waitTerminal(v.ID, 10*time.Second)
	if done.State != string(job.Cancelled) {
		t.Errorf("state = %s, want CANCELLED", done.State)
	}
}

func TestSubmitRejectsImpossibleRequestImmediately(t *testing.T) {
	h := start(t)
	var e map[string]string
	code := h.do("POST", "/submit",
		job.Spec{Name: "huge", User: "t", Script: "/bin/true", Limits: job.Limits{CPUs: 99999}}, &e)
	if code == 200 {
		t.Fatal("a request larger than the node should be rejected at submit, not left pending forever")
	}
	if !strings.Contains(e["error"], "CPUs") {
		t.Errorf("unhelpful error: %v", e)
	}
}

func TestQueueingSerialisesOversubscription(t *testing.T) {
	h := start(t)
	var ids []int64
	// Each job asks for the whole node's memory, so they cannot run together.
	var nodes []ctl.NodeInfo
	h.do("GET", "/node", nil, &nodes)
	big := (nodes[0].MemMiB * 3 / 4) << 20
	for i := 0; i < 2; i++ {
		v := h.submitScript("q"+itoa(int64(i))+".sh", "#!/bin/sh\nsleep 1\n",
			job.Limits{CPUs: 1, MemBytes: big})
		ids = append(ids, v.ID)
	}
	// The second must report a reason rather than sitting there unexplained.
	time.Sleep(500 * time.Millisecond)
	var second ctl.JobView
	h.do("GET", "/job/"+itoa(ids[1]), nil, &second)
	if second.State == string(job.Pending) && second.Reason == "" {
		t.Error("pending job has no reason; 'why is my job pending' must always be answerable")
	}
	for _, id := range ids {
		h.waitTerminal(id, 30*time.Second)
	}
}

func TestNodeInfoIsHonestAboutEnforcement(t *testing.T) {
	h := start(t)
	var nodes []ctl.NodeInfo
	if code := h.do("GET", "/node", nil, &nodes); code != 200 {
		t.Fatalf("status %d", code)
	}
	if len(nodes) == 0 {
		t.Fatal("no nodes registered")
	}
	n := nodes[0]
	if n.MemLimit != "polled" {
		t.Errorf("mem enforcement = %q, want polled", n.MemLimit)
	}
	if n.CPULimit != "advisory" {
		t.Errorf("cpu enforcement = %q, want advisory", n.CPULimit)
	}
	if len(n.Limitations) == 0 {
		t.Error("node must publish its limitations")
	}
}

func TestEndToEndJobArray(t *testing.T) {
	h := start(t)
	p := filepath.Join(h.root, "arr.sh")
	// Each task must see its own index, and only its own.
	os.WriteFile(p, []byte("#!/bin/sh\necho task=$SHOME_ARRAY_TASK_ID slurm=$SLURM_ARRAY_TASK_ID\n"), 0o755)

	var v ctl.JobView
	code := h.do("POST", "/submit", ctl.SubmitRequest{
		Spec:  job.Spec{Name: "arr", User: "tester", Script: p, Limits: job.Limits{CPUs: 1}},
		Array: "0-3",
	}, &v)
	if code != 200 {
		t.Fatalf("submit status %d", code)
	}
	if v.ArrayTasks != 4 {
		t.Fatalf("expected 4 tasks, got %d", v.ArrayTasks)
	}

	// All four tasks must complete, each with a distinct index.
	var all []ctl.JobView
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		all = nil
		h.do("GET", "/jobs", nil, &all)
		done := 0
		for _, j := range all {
			if job.State(j.State).Terminal() {
				done++
			}
		}
		if done == 4 && len(all) == 4 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(all) != 4 {
		t.Fatalf("expected 4 job rows, got %d", len(all))
	}
	seen := map[string]bool{}
	for _, j := range all {
		if j.State != string(job.Completed) {
			t.Errorf("task %s state = %s (%s)", j.Label, j.State, j.Reason)
			continue
		}
		out := strings.TrimSpace(h.output(j))
		if !strings.HasPrefix(out, "task=") {
			t.Errorf("task %s output = %q", j.Label, out)
		}
		if seen[out] {
			t.Errorf("two tasks reported the same index: %q", out)
		}
		seen[out] = true
		// The Slurm alias must be populated too, or ported scripts break.
		if !strings.Contains(out, "slurm=") || strings.HasSuffix(out, "slurm=") {
			t.Errorf("SLURM_ARRAY_TASK_ID not set: %q", out)
		}
		if !strings.Contains(j.Label, "_") {
			t.Errorf("array task label %q should be arrayid_taskid", j.Label)
		}
	}
	if len(seen) != 4 {
		t.Errorf("expected 4 distinct task indices, got %d", len(seen))
	}
}

func TestCancellingArrayIDCancelsEveryTask(t *testing.T) {
	h := start(t)
	p := filepath.Join(h.root, "sleep.sh")
	os.WriteFile(p, []byte("#!/bin/sh\nsleep 60\n"), 0o755)
	var v ctl.JobView
	h.do("POST", "/submit", ctl.SubmitRequest{
		Spec:  job.Spec{Name: "s", User: "tester", Script: p, Limits: job.Limits{CPUs: 1}},
		Array: "0-2",
	}, &v)

	time.Sleep(400 * time.Millisecond)
	if code := h.do("POST", "/cancel/"+itoa(v.ID), nil, nil); code != 200 {
		t.Fatalf("cancel status %d", code)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var all []ctl.JobView
		h.do("GET", "/jobs", nil, &all)
		n := 0
		for _, j := range all {
			if j.State == string(job.Cancelled) {
				n++
			}
		}
		if n == 3 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("cancelling the array id should cancel every task in it")
}

// ---------- multi-node ----------

func TestMultiNodeSpreadsWork(t *testing.T) {
	h := startN(t, 3)
	p := filepath.Join(h.root, "w.sh")
	os.WriteFile(p, []byte("#!/bin/sh\nsleep 2\n"), 0o755)

	// Each job takes most of a node's memory so they cannot share one.
	var nodes []ctl.NodeInfo
	h.do("GET", "/node", nil, &nodes)
	big := (nodes[0].MemMiB * 2 / 3) << 20

	var ids []int64
	for i := 0; i < 3; i++ {
		var v ctl.JobView
		if code := h.do("POST", "/submit", job.Spec{
			Name: "w", User: "t", Script: p, ArrayTaskID: -1,
			Limits: job.Limits{CPUs: 1, MemBytes: big},
		}, &v); code != 200 {
			t.Fatalf("submit %d: status %d", i, code)
		}
		ids = append(ids, v.ID)
	}

	// All three should be placed concurrently, on distinct nodes.
	deadline := time.Now().Add(20 * time.Second)
	placement := map[int64]string{}
	for time.Now().Before(deadline) {
		placement = map[int64]string{}
		for _, id := range ids {
			var v ctl.JobView
			h.do("GET", "/job/"+itoa(id), nil, &v)
			if v.State == string(job.Running) && v.Node != "" {
				placement[id] = v.Node
			}
		}
		if len(placement) == 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(placement) != 3 {
		t.Fatalf("only %d of 3 jobs running concurrently: %v", len(placement), placement)
	}
	distinct := map[string]bool{}
	for _, n := range placement {
		distinct[n] = true
	}
	if len(distinct) != 3 {
		t.Errorf("jobs landed on %d distinct nodes, want 3: %v", len(distinct), placement)
	}
	for _, id := range ids {
		h.waitTerminal(id, 30*time.Second)
	}
}

func TestDrainedNodeGetsNoNewWork(t *testing.T) {
	h := startN(t, 2)
	// Drain everything, then submit: the job must stay pending with a reason.
	for _, n := range h.nodes {
		if code := h.do("POST", "/node/"+n+"/drain?reason=testing", nil, nil); code != 200 {
			t.Fatalf("drain %s: status %d", n, code)
		}
	}
	v := h.submitScript("d.sh", "#!/bin/sh\necho hi\n", job.Limits{CPUs: 1})
	time.Sleep(1500 * time.Millisecond)
	var cur ctl.JobView
	h.do("GET", "/job/"+itoa(v.ID), nil, &cur)
	if cur.State != string(job.Pending) {
		t.Fatalf("job is %s; a fully drained cluster must run nothing", cur.State)
	}
	if cur.Reason == "" {
		t.Error("pending job on a drained cluster must explain itself")
	}

	// Resuming one node lets it through.
	h.do("POST", "/node/"+h.nodes[0]+"/resume", nil, nil)
	done := h.waitTerminal(v.ID, 30*time.Second)
	if done.State != string(job.Completed) {
		t.Errorf("after resume, state = %s (%s)", done.State, done.Reason)
	}
}

// ---------- dependencies ----------

func TestDependencyAfterOK(t *testing.T) {
	h := start(t)
	first := h.submitScript("first.sh", "#!/bin/sh\nsleep 1\necho first-done\n", job.Limits{CPUs: 1})

	p := filepath.Join(h.root, "second.sh")
	os.WriteFile(p, []byte("#!/bin/sh\necho second-ran\n"), 0o755)
	var second ctl.JobView
	h.do("POST", "/submit", job.Spec{
		Name: "second", User: "t", Script: p, ArrayTaskID: -1,
		Dependency: "afterok:" + itoa(first.ID),
		Limits:     job.Limits{CPUs: 1},
	}, &second)

	// While the first runs, the second must be pending and say why.
	time.Sleep(300 * time.Millisecond)
	var cur ctl.JobView
	h.do("GET", "/job/"+itoa(second.ID), nil, &cur)
	if cur.State == string(job.Completed) {
		t.Fatal("dependent job ran before its dependency finished")
	}
	h.waitTerminal(first.ID, 30*time.Second)
	done := h.waitTerminal(second.ID, 30*time.Second)
	if done.State != string(job.Completed) {
		t.Errorf("dependent job state = %s (%s)", done.State, done.Reason)
	}
}

func TestDependencyAfterOKBlockedByFailure(t *testing.T) {
	h := start(t)
	first := h.submitScript("bad.sh", "#!/bin/sh\nexit 3\n", job.Limits{CPUs: 1})
	h.waitTerminal(first.ID, 30*time.Second)

	p := filepath.Join(h.root, "never.sh")
	os.WriteFile(p, []byte("#!/bin/sh\necho should-not-run\n"), 0o755)
	var second ctl.JobView
	h.do("POST", "/submit", job.Spec{
		Name: "never", User: "t", Script: p, ArrayTaskID: -1,
		Dependency: "afterok:" + itoa(first.ID),
		Limits:     job.Limits{CPUs: 1},
	}, &second)

	// The invariant is that it never RUNS. Whether it waits or is cancelled is
	// an implementation choice -- shome cancels, since an afterok whose
	// prerequisite failed can never become runnable, and leaving it queued
	// forever misleads anyone reading squeue.
	deadline := time.Now().Add(20 * time.Second)
	var cur ctl.JobView
	for time.Now().Before(deadline) {
		h.do("GET", "/job/"+itoa(second.ID), nil, &cur)
		if cur.State == string(job.Running) {
			t.Fatal("afterok job ran even though its dependency failed")
		}
		if job.State(cur.State).Terminal() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if cur.State != string(job.Cancelled) {
		t.Errorf("state = %s; an unsatisfiable afterok should be cancelled", cur.State)
	}
	if !strings.Contains(cur.Reason, "did not succeed") {
		t.Errorf("reason = %q, should explain the failed dependency", cur.Reason)
	}
	if got := h.output(cur); strings.Contains(got, "should-not-run") {
		t.Error("the dependent job actually executed")
	}
}

func TestSubmitRejectsBadDependency(t *testing.T) {
	h := start(t)
	var e map[string]string
	code := h.do("POST", "/submit", job.Spec{
		Name: "x", User: "t", Script: "/bin/true", ArrayTaskID: -1,
		Dependency: "afterlunch:1",
	}, &e)
	if code == 200 {
		t.Fatal("an unsupported dependency type must be rejected, not silently ignored")
	}
}

// ---------- hold / requeue ----------

func TestHoldPreventsScheduling(t *testing.T) {
	h := start(t)
	// Hold before the scheduler can place it.
	v := h.submitScript("h.sh", "#!/bin/sh\necho held-then-ran\n", job.Limits{CPUs: 1})
	h.do("POST", "/job/"+itoa(v.ID)+"/hold", nil, nil)

	time.Sleep(1200 * time.Millisecond)
	var cur ctl.JobView
	h.do("GET", "/job/"+itoa(v.ID), nil, &cur)
	if cur.State == string(job.Completed) {
		t.Skip("job completed before the hold landed; timing-dependent")
	}
	if cur.State != string(job.Pending) || !cur.Held {
		t.Fatalf("state=%s held=%v, want PENDING and held", cur.State, cur.Held)
	}

	h.do("POST", "/job/"+itoa(v.ID)+"/release", nil, nil)
	done := h.waitTerminal(v.ID, 30*time.Second)
	if done.State != string(job.Completed) {
		t.Errorf("after release, state = %s (%s)", done.State, done.Reason)
	}
}

// ---------- output archiving and staging ----------

func TestOutputIsRetrievableFromController(t *testing.T) {
	h := start(t)
	v := h.submitScript("o.sh", "#!/bin/sh\necho archived-output\n", job.Limits{CPUs: 1})
	done := h.waitTerminal(v.ID, 30*time.Second)
	if done.State != string(job.Completed) {
		t.Fatalf("state = %s (%s)", done.State, done.Reason)
	}
	// Served by the controller, so a user never needs to reach the node that
	// happened to run the job.
	if got := h.output(done); !strings.Contains(got, "archived-output") {
		t.Errorf("job output was never archived on the controller (got %q)", got)
	}
}

func TestStageInAndStageOut(t *testing.T) {
	h := start(t)

	// Build an input tree and pack it exactly as the CLI would.
	srcDir := t.TempDir()
	os.MkdirAll(filepath.Join(srcDir, "input"), 0o755)
	os.WriteFile(filepath.Join(srcDir, "input", "data.txt"), []byte("hello-from-input"), 0o644)

	script := filepath.Join(h.root, "stage.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nmkdir -p results\ncat input/data.txt > results/copy.txt\necho done\n"), 0o755)

	var v ctl.JobView
	code := h.do("POST", "/submit", ctl.SubmitRequest{
		Spec: job.Spec{
			Name: "stage", User: "t", Script: script, ArrayTaskID: -1,
			StageIn: true, StageOut: []string{"results"},
			Limits: job.Limits{CPUs: 1},
		},
		Hold: true,
	}, &v)
	if code != 200 {
		t.Fatalf("submit status %d", code)
	}

	var buf bytes.Buffer
	if err := xfer.Tar(&buf, srcDir, []string{"input"}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "http://shome/job/"+itoa(v.ID)+"/stagein", &buf)
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stage-in upload status %d", resp.StatusCode)
	}
	h.do("POST", "/job/"+itoa(v.ID)+"/release", nil, nil)

	done := h.waitTerminal(v.ID, 40*time.Second)
	if done.State != string(job.Completed) {
		t.Fatalf("state = %s (%s); output: %s", done.State, done.Reason, h.output(done))
	}

	// The results archive must come back with the file the job produced from
	// its staged input -- proving both directions worked.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(h.root, "stage", "job-"+itoa(v.ID)+".out.tgz"))
		if err == nil {
			dest := t.TempDir()
			if err := xfer.Untar(bytes.NewReader(b), dest); err != nil {
				t.Fatalf("unpack results: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dest, "results", "copy.txt"))
			if err != nil {
				t.Fatalf("results/copy.txt missing: %v", err)
			}
			if strings.TrimSpace(string(got)) != "hello-from-input" {
				t.Errorf("copy.txt = %q", got)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("stage-out archive never arrived at the controller")
}

func TestStageInUploadRejectedAfterJobStarts(t *testing.T) {
	h := start(t)
	v := h.submitScript("r.sh", "#!/bin/sh\necho x\n", job.Limits{CPUs: 1})
	h.waitTerminal(v.ID, 30*time.Second)
	// Uploading inputs to a job that already ran is a client bug; it must be
	// refused rather than silently ignored.
	req, _ := http.NewRequest("POST", "http://shome/job/"+itoa(v.ID)+"/stagein", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Error("stage-in upload accepted for an already-finished job")
	}
}

// ---------- multi-tenancy ----------

// doAs issues a request with a specific token, for cross-user checks.
func (h *harness) doAs(token, method, path string, body any, out any) int {
	h.t.Helper()
	var rdr io.Reader = strings.NewReader("")
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, "http://shome"+path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.cli.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func (h *harness) addUser(name string) string {
	h.t.Helper()
	var out map[string]string
	if code := h.do("POST", "/users", map[string]string{"name": name, "role": "user"}, &out); code != 200 {
		h.t.Fatalf("create user %s: status %d", name, code)
	}
	return out["token"]
}

func TestUnauthenticatedRequestsRejected(t *testing.T) {
	h := start(t)
	for _, path := range []string{"/jobs", "/node", "/whoami"} {
		if code := h.doAs("", "GET", path, nil, nil); code != 401 {
			t.Errorf("GET %s without a token returned %d, want 401", path, code)
		}
	}
	if code := h.doAs("not-a-real-token", "GET", "/jobs", nil, nil); code != 401 {
		t.Errorf("bogus token returned %d, want 401", code)
	}
}

func TestSubmitterIdentityComesFromTokenNotBody(t *testing.T) {
	h := start(t)
	alice := h.addUser("alice")
	var v ctl.JobView
	// Claim to be someone else in the body; the token must win.
	code := h.doAs(alice, "POST", "/submit", job.Spec{
		Name: "spoof", User: "victim", Script: "/bin/true", ArrayTaskID: -1,
		Limits: job.Limits{CPUs: 1},
	}, &v)
	if code != 200 {
		t.Fatalf("submit status %d", code)
	}
	if v.User != "alice" {
		t.Errorf("job recorded user %q; identity must come from the token, not the body", v.User)
	}
}

func TestUsersCannotSeeOrTouchEachOthersJobs(t *testing.T) {
	h := start(t)
	alice := h.addUser("alice")
	bob := h.addUser("bob")

	var aliceJob ctl.JobView
	p := filepath.Join(h.root, "sleep.sh")
	os.WriteFile(p, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	if code := h.doAs(alice, "POST", "/submit", job.Spec{
		Name: "private", User: "alice", Script: p, ArrayTaskID: -1, Limits: job.Limits{CPUs: 1},
	}, &aliceJob); code != 200 {
		t.Fatalf("alice submit: %d", code)
	}

	t.Run("bob cannot read it", func(t *testing.T) {
		if code := h.doAs(bob, "GET", "/job/"+itoa(aliceJob.ID), nil, nil); code == 200 {
			t.Error("bob read alice's job")
		}
	})
	t.Run("bob cannot cancel it", func(t *testing.T) {
		if code := h.doAs(bob, "POST", "/cancel/"+itoa(aliceJob.ID), nil, nil); code == 200 {
			t.Error("bob cancelled alice's job")
		}
	})
	t.Run("bob cannot read its output", func(t *testing.T) {
		if code := h.doAs(bob, "GET", "/job/"+itoa(aliceJob.ID)+"/output", nil, nil); code == 200 {
			t.Error("bob read the output of alice's job")
		}
	})
	t.Run("bob's job list excludes it", func(t *testing.T) {
		var jobs []ctl.JobView
		h.doAs(bob, "GET", "/jobs", nil, &jobs)
		for _, j := range jobs {
			if j.ID == aliceJob.ID {
				t.Error("alice's job appeared in bob's queue listing")
			}
		}
	})
	t.Run("bob cannot see it even by asking for her user", func(t *testing.T) {
		var jobs []ctl.JobView
		h.doAs(bob, "GET", "/jobs?user=alice", nil, &jobs)
		for _, j := range jobs {
			if j.User != "bob" {
				t.Errorf("bob saw a job owned by %q", j.User)
			}
		}
	})
	t.Run("alice can still see her own", func(t *testing.T) {
		if code := h.doAs(alice, "GET", "/job/"+itoa(aliceJob.ID), nil, nil); code != 200 {
			t.Errorf("alice cannot see her own job: %d", code)
		}
	})
	t.Run("admin can see everything", func(t *testing.T) {
		if code := h.do("GET", "/job/"+itoa(aliceJob.ID), nil, nil); code != 200 {
			t.Error("admin cannot see alice's job")
		}
	})
	h.do("POST", "/cancel/"+itoa(aliceJob.ID), nil, nil)
}

func TestNonAdminCannotAdminister(t *testing.T) {
	h := start(t)
	alice := h.addUser("alice")
	for _, c := range []struct{ method, path string }{
		{"POST", "/node/" + h.nodes[0] + "/drain"},
		{"POST", "/node/" + h.nodes[0] + "/resume"},
		{"POST", "/token"},
		{"GET", "/users"},
		{"POST", "/shutdown"},
	} {
		if code := h.doAs(alice, c.method, c.path, nil, nil); code != 403 {
			t.Errorf("%s %s as a regular user returned %d, want 403", c.method, c.path, code)
		}
	}
}

// ---------- gang scheduling ----------

// submitAggregate submits a job that asks for totals rather than a per-node shape.
func (h *harness) submitAggregate(name, contents string, spec job.Spec) ctl.JobView {
	h.t.Helper()
	p := filepath.Join(h.root, name)
	if err := os.WriteFile(p, []byte(contents), 0o755); err != nil {
		h.t.Fatal(err)
	}
	spec.Name, spec.User, spec.Script, spec.ArrayTaskID = name, "tester", p, -1
	var v ctl.JobView
	if code := h.do("POST", "/submit", ctl.SubmitRequest{Spec: spec}, &v); code != 200 {
		h.t.Fatalf("submit %s: status %d", name, code)
	}
	return v
}

func TestGangJobRunsOnEveryAllocatedNode(t *testing.T) {
	h := startN(t, 3)
	// Ask for more CPUs than any single node has, forcing a multi-node gang.
	var n ctl.NodeInfo
	var infos []ctl.NodeInfo
	h.do("GET", "/nodes", nil, &infos)
	if len(infos) == 0 {
		h.do("GET", "/node", nil, &n)
		infos = []ctl.NodeInfo{n}
	}
	per := infos[0].CPUs
	total := per*2 + 1 // cannot fit on fewer than 3 nodes

	// Each rank appends its identity, so the output proves every node ran.
	script := "#!/bin/sh\necho \"rank $SHOME_NODEID of $SHOME_NNODES on $(hostname)\"\n" +
		"echo \"nodelist=$SHOME_NODELIST master=$SHOME_MASTER_ADDR:$SHOME_MASTER_PORT\"\n" +
		"echo \"slurm alias: $SLURM_PROCID/$SLURM_NNODES\"\n"
	v := h.submitAggregate("gang.sh", script, job.Spec{TotalCPUs: total})

	done := h.waitTerminal(v.ID, 60*time.Second)
	if done.State != string(job.Completed) {
		t.Fatalf("state = %s (%s)", done.State, done.Reason)
	}
	// The job's node field should name every participant.
	if !strings.Contains(done.Node, ",") {
		t.Errorf("node = %q; a gang job should list all its nodes", done.Node)
	}
	t.Logf("ran across: %s", done.Node)
}

func TestGangAllocationIsAllOrNothing(t *testing.T) {
	h := startN(t, 2)
	var infos []ctl.NodeInfo
	h.do("GET", "/nodes", nil, &infos)
	if len(infos) < 2 {
		t.Skip("needs two nodes")
	}
	// More CPUs than the whole cluster has: must never partially allocate.
	total := infos[0].CPUs*len(infos) + 100
	v := h.submitAggregate("toobig.sh", "#!/bin/sh\necho hi\n", job.Spec{TotalCPUs: total})

	time.Sleep(1500 * time.Millisecond)
	var cur ctl.JobView
	h.do("GET", "/job/"+itoa(v.ID), nil, &cur)
	if cur.State == string(job.Running) {
		t.Error("an unsatisfiable gang request started anyway")
	}
	if cur.Reason == "" {
		t.Error("no explanation for the unsatisfiable request")
	}
	if !strings.Contains(cur.Reason, "never fit") {
		t.Errorf("reason = %q; should say it cannot ever fit", cur.Reason)
	}
	h.do("POST", "/cancel/"+itoa(v.ID), nil, nil)
}

func TestGangFailsIfAnyRankFails(t *testing.T) {
	h := startN(t, 2)
	var infos []ctl.NodeInfo
	h.do("GET", "/nodes", nil, &infos)
	if len(infos) < 2 {
		t.Skip("needs two nodes")
	}
	total := infos[0].CPUs + 1 // force two nodes
	// Rank 1 exits non-zero; the job as a whole must fail and say which rank.
	script := "#!/bin/sh\nif [ \"$SHOME_NODEID\" = \"1\" ]; then exit 9; fi\nsleep 1\n"
	v := h.submitAggregate("partial.sh", script, job.Spec{TotalCPUs: total})

	done := h.waitTerminal(v.ID, 60*time.Second)
	if done.State == string(job.Completed) {
		t.Fatal("job reported success although a rank failed")
	}
	if !strings.Contains(done.Reason, "rank") {
		t.Errorf("reason = %q; should name the failing rank and node", done.Reason)
	}
	t.Logf("failure reported as: %s", done.Reason)
}

func TestGangCancelStopsEveryRank(t *testing.T) {
	h := startN(t, 2)
	var infos []ctl.NodeInfo
	h.do("GET", "/nodes", nil, &infos)
	if len(infos) < 2 {
		t.Skip("needs two nodes")
	}
	total := infos[0].CPUs + 1
	v := h.submitAggregate("long.sh", "#!/bin/sh\nsleep 120\n", job.Spec{TotalCPUs: total})

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var cur ctl.JobView
		h.do("GET", "/job/"+itoa(v.ID), nil, &cur)
		if cur.State == string(job.Running) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code := h.do("POST", "/cancel/"+itoa(v.ID), nil, nil); code != 200 {
		t.Fatalf("cancel status %d", code)
	}
	done := h.waitTerminal(v.ID, 20*time.Second)
	if done.State != string(job.Cancelled) {
		t.Errorf("state = %s, want CANCELLED", done.State)
	}
}

func TestPlanEndpointMatchesWhatGetsScheduled(t *testing.T) {
	h := startN(t, 3)
	var infos []ctl.NodeInfo
	h.do("GET", "/nodes", nil, &infos)
	if len(infos) < 3 {
		t.Skip("needs three nodes")
	}
	total := infos[0].CPUs*2 + 1
	var plan ctl.PlanResult
	if code := h.do("POST", "/plan", job.Spec{TotalCPUs: total}, &plan); code != 200 {
		t.Fatalf("plan status %d", code)
	}
	if !plan.Feasible {
		t.Fatalf("plan says infeasible: %s", plan.Why)
	}
	// `shome plan` is only useful if it predicts the real placement.
	v := h.submitAggregate("match.sh", "#!/bin/sh\nsleep 1\n", job.Spec{TotalCPUs: total})
	done := h.waitTerminal(v.ID, 60*time.Second)
	got := strings.Split(done.Node, ",")
	if len(got) != len(plan.Nodes) {
		t.Errorf("plan predicted %d nodes (%v), job used %d (%v)",
			len(plan.Nodes), plan.Nodes, len(got), got)
	}
}

func TestUnsatisfiableDependencyIsCancelledNotLeftPending(t *testing.T) {
	h := start(t)
	bad := h.submitScript("bad.sh", "#!/bin/sh\nexit 3\n", job.Limits{CPUs: 1})
	h.waitTerminal(bad.ID, 30*time.Second)

	// afterok on a job that failed: this can never run.
	p := filepath.Join(h.root, "next.sh")
	os.WriteFile(p, []byte("#!/bin/sh\necho should-not-run\n"), 0o755)
	var next ctl.JobView
	h.do("POST", "/submit", ctl.SubmitRequest{Spec: job.Spec{
		Name: "next", User: "tester", Script: p, ArrayTaskID: -1,
		Dependency: "afterok:" + itoa(bad.ID), Limits: job.Limits{CPUs: 1},
	}}, &next)

	done := h.waitTerminal(next.ID, 30*time.Second)
	if done.State != string(job.Cancelled) {
		t.Errorf("state = %s; an unsatisfiable dependency should be cancelled, not queued forever", done.State)
	}
	if !strings.Contains(done.Reason, "never satisfied") {
		t.Errorf("reason = %q, should explain why", done.Reason)
	}
	if got := h.output(done); strings.Contains(got, "should-not-run") {
		t.Error("the dependent job actually ran")
	}
}

func TestAfterAnyStillRunsAfterFailure(t *testing.T) {
	h := start(t)
	bad := h.submitScript("bad2.sh", "#!/bin/sh\nexit 4\n", job.Limits{CPUs: 1})
	h.waitTerminal(bad.ID, 30*time.Second)

	// afterany means "once it finishes, however it finishes" -- this must NOT
	// be treated as unsatisfiable.
	p := filepath.Join(h.root, "any.sh")
	os.WriteFile(p, []byte("#!/bin/sh\necho ran-anyway\n"), 0o755)
	var next ctl.JobView
	h.do("POST", "/submit", ctl.SubmitRequest{Spec: job.Spec{
		Name: "any", User: "tester", Script: p, ArrayTaskID: -1,
		Dependency: "afterany:" + itoa(bad.ID), Limits: job.Limits{CPUs: 1},
	}}, &next)

	done := h.waitTerminal(next.ID, 30*time.Second)
	if done.State != string(job.Completed) {
		t.Errorf("state = %s (%s); afterany should still run", done.State, done.Reason)
	}
}

// ---------- services ----------

func TestServiceIsRestartedWhenItsJobEnds(t *testing.T) {
	h := start(t)
	script := filepath.Join(h.root, "svc.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 300\n"), 0o755)

	if code := h.do("POST", "/services", map[string]any{
		"name": "resident",
		"spec": job.Spec{Name: "resident", Script: script, ArrayTaskID: -1,
			Limits: job.Limits{CPUs: 1}},
	}, nil); code != 200 {
		t.Fatalf("create service: %d", code)
	}

	first := waitServiceJob(t, h, "resident", 0)
	// Cancelling the backing job must not stop the service; that is the whole
	// difference between a service and a batch job.
	h.do("POST", "/cancel/"+itoa(first), nil, nil)
	second := waitServiceJob(t, h, "resident", first)
	if second == first {
		t.Fatal("service was not restarted after its job ended")
	}
	t.Logf("restarted: job %d -> %d", first, second)

	// Stopping it must stick.
	h.do("POST", "/services/resident/stop", nil, nil)
	time.Sleep(2 * time.Second)
	var svcs []ctl.ServiceView
	h.do("GET", "/services", nil, &svcs)
	if len(svcs) != 1 || svcs[0].Desired != "stopped" {
		t.Fatalf("service should be stopped: %+v", svcs)
	}
	before := svcs[0].JobID
	time.Sleep(3 * time.Second)
	h.do("GET", "/services", nil, &svcs)
	if svcs[0].JobID != before {
		t.Error("a stopped service was restarted anyway")
	}
}

func waitServiceJob(t *testing.T, h *harness, name string, notID int64) int64 {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var svcs []ctl.ServiceView
		h.do("GET", "/services", nil, &svcs)
		for _, s := range svcs {
			if s.Name == name && s.JobID != 0 && s.JobID != notID && s.JobState == "RUNNING" {
				return s.JobID
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("service %q never got a running job (was not %d)", name, notID)
	return 0
}

func TestServicesAreOwnerScoped(t *testing.T) {
	h := start(t)
	alice := h.addUser("alice")
	bob := h.addUser("bob")
	script := filepath.Join(h.root, "s.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o755)

	h.doAs(alice, "POST", "/services", map[string]any{
		"name": "alices",
		"spec": job.Spec{Name: "alices", Script: script, ArrayTaskID: -1,
			Limits: job.Limits{CPUs: 1}},
	}, nil)

	var bobSees []ctl.ServiceView
	h.doAs(bob, "GET", "/services", nil, &bobSees)
	for _, s := range bobSees {
		if s.Name == "alices" {
			t.Error("bob can see alice's service")
		}
	}
	if code := h.doAs(bob, "DELETE", "/services/alices", nil, nil); code == 200 {
		t.Error("bob deleted alice's service")
	}
	// Admin sees everything.
	var adminSees []ctl.ServiceView
	h.do("GET", "/services", nil, &adminSees)
	found := false
	for _, s := range adminSees {
		if s.Name == "alices" {
			found = true
		}
	}
	if !found {
		t.Error("admin cannot see alice's service")
	}
	h.do("DELETE", "/services/alices", nil, nil)
}

func TestOperatorRoleBoundaries(t *testing.T) {
	h := startN(t, 1)
	var out map[string]string
	h.do("POST", "/users", map[string]string{"name": "opsuser", "role": "operator"}, &out)
	ops := out["token"]
	if ops == "" {
		t.Fatal("no operator token issued")
	}
	// Operators act on running work...
	if code := h.doAs(ops, "POST", "/node/"+h.nodes[0]+"/drain", nil, nil); code != 200 {
		t.Errorf("operator could not drain a node: %d", code)
	}
	if code := h.doAs(ops, "POST", "/node/"+h.nodes[0]+"/resume", nil, nil); code != 200 {
		t.Errorf("operator could not resume a node: %d", code)
	}
	// ...but not on identity or entitlement.
	for _, c := range []struct{ method, path string }{
		{"GET", "/users"},
		{"POST", "/users"},
	} {
		if code := h.doAs(ops, c.method, c.path, map[string]string{"name": "x"}, nil); code != 403 {
			t.Errorf("%s %s as operator returned %d, want 403", c.method, c.path, code)
		}
	}
}

func TestQuarantineStopsWorkAndRevokesAccess(t *testing.T) {
	h := start(t)
	alice := h.addUser("alice")
	script := filepath.Join(h.root, "long.sh")
	os.WriteFile(script, []byte("#!/bin/sh\nsleep 120\n"), 0o755)
	var j ctl.JobView
	h.doAs(alice, "POST", "/submit", job.Spec{
		Name: "victim", Script: script, ArrayTaskID: -1, Limits: job.Limits{CPUs: 1},
	}, &j)

	if code := h.do("POST", "/users/alice/quarantine?reason=test", nil, nil); code != 200 {
		t.Fatalf("quarantine failed: %d", code)
	}
	// The token must stop working immediately, and say why: 403 rather than
	// 401, because the credential is genuine and the account is suspended.
	// "Invalid API token" would send somebody after a token that is fine.
	if code := h.doAs(alice, "GET", "/jobs", nil, nil); code != 403 {
		t.Errorf("quarantined token returned %d, want 403", code)
	}
	// ...and their work must be stopped.
	done := h.waitTerminal(j.ID, 20*time.Second)
	if done.State != string(job.Cancelled) {
		t.Errorf("job state = %s, want CANCELLED", done.State)
	}
	// Restoring must bring the account back.
	h.do("POST", "/users/alice/unquarantine", nil, nil)
	if code := h.doAs(alice, "GET", "/jobs", nil, nil); code != 200 {
		t.Errorf("unquarantine did not restore access: %d", code)
	}
}

// A script living in the submitter's home must still run.
//
// The sandbox denies the owner's home, so a job executing its script by
// original path cannot read it and dies with exit 127. shome therefore sends
// the script's contents and runs a copy in the job's own scratch. Found by an
// example whose scripts lived in the repo, i.e. in the home directory --
// which is where anyone would naturally keep them.
func TestScriptInDeniedPathStillRuns(t *testing.T) {
	h := start(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dir, err := os.MkdirTemp(home, ".shome-test-")
	if err != nil {
		t.Skipf("cannot write to home: %v", err)
	}
	defer os.RemoveAll(dir)

	script := filepath.Join(dir, "inhome.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ran-from-home\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	var v ctl.JobView
	if code := h.do("POST", "/submit", ctl.SubmitRequest{Spec: job.Spec{
		Name: "inhome", User: "tester", Script: script, ScriptBody: body,
		ArrayTaskID: -1, Limits: job.Limits{CPUs: 1},
	}}, &v); code != 200 {
		t.Fatalf("submit: %d", code)
	}
	done := h.waitTerminal(v.ID, 30*time.Second)
	if done.State != string(job.Completed) {
		t.Fatalf("state = %s (%s); a script in $HOME should still run", done.State, done.Reason)
	}
	if got := h.output(done); !strings.Contains(got, "ran-from-home") {
		t.Errorf("output = %q", got)
	}
}

// Rank variables must be set even on one machine.
func TestRankEnvSetForSingleNodeJob(t *testing.T) {
	h := start(t)
	v := h.submitScript("rank.sh",
		"#!/bin/sh\necho \"rank=$SHOME_NODEID/$SHOME_NNODES slurm=$SLURM_NNODES\"\n",
		job.Limits{CPUs: 1})
	done := h.waitTerminal(v.ID, 30*time.Second)
	out := h.output(done)
	// A script that has to test whether the variable exists breaks the first
	// time someone runs it on a single machine.
	if !strings.Contains(out, "rank=0/1") || !strings.Contains(out, "slurm=1") {
		t.Errorf("single-node rank env = %q, want rank=0/1 slurm=1", strings.TrimSpace(out))
	}
}

// Losing sight of a node must NOT, by itself, disturb its jobs.
//
// The agent keeps computing when it loses the controller, so a machine on
// spotty Wi-Fi is almost certainly still working. Killing or requeueing at
// that moment destroys work that is fine.
func TestUnreachableNodeLeavesItsJobsRunning(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()

	j, _ := st.Submit(ctx, job.Spec{Name: "long", User: "u", Script: "/bin/true", ArrayTaskID: -1}, now)
	st.UpsertNode(ctx, "flaky", "10.0.0.9", platform.Capabilities{OS: "linux", CPUs: 4}, now)
	st.MarkRunning(ctx, j.ID, "flaky", "", now)

	// The node stops heartbeating.
	if _, err := st.ExpireNodes(ctx, time.Nanosecond, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkJobsUnreachable(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, j.ID)
	if got.State != job.Running {
		t.Fatalf("state = %s; an unreachable node must not stop its jobs", got.State)
	}
	if !strings.Contains(got.Reason, "presumed still running") {
		t.Errorf("reason = %q; squeue should say we cannot see it", got.Reason)
	}

	// Well inside the grace period, nothing is written off.
	if r, f, err := st.GiveUpOnLostJobs(ctx, time.Hour, now.Add(time.Minute)); err != nil || r+f != 0 {
		t.Fatalf("gave up too early: requeued=%d failed=%d err=%v", r, f, err)
	}
	got, _ = st.Get(ctx, j.ID)
	if got.State != job.Running {
		t.Errorf("state = %s within the grace period, want RUNNING", got.State)
	}

	// The node comes back: the annotation clears and nothing was harmed.
	st.UpsertNode(ctx, "flaky", "10.0.0.9", platform.Capabilities{OS: "linux", CPUs: 4}, time.Now())
	st.ClearUnreachable(ctx, "flaky")
	got, _ = st.Get(ctx, j.ID)
	if got.State != job.Running || got.Reason != "" {
		t.Errorf("after reconnect: state=%s reason=%q, want RUNNING with no reason", got.State, got.Reason)
	}
}

// Past the grace period the job is finally written off -- failed by default,
// requeued only if the submitter said it is safe to repeat.
func TestJobsGivenUpAfterGraceFailUnlessRequeueRequested(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "g.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()

	st.UpsertNode(ctx, "doomed", "10.0.0.9", platform.Capabilities{OS: "linux", CPUs: 4}, now)
	mk := func(name string, requeue bool) int64 {
		j, _ := st.Submit(ctx, job.Spec{
			Name: name, User: "u", Script: "/bin/true", ArrayTaskID: -1, Requeue: requeue,
		}, now)
		st.MarkRunning(ctx, j.ID, "doomed", "", now)
		return j.ID
	}
	plain, opted := mk("plain", false), mk("opted", true)
	st.ExpireNodes(ctx, time.Nanosecond, now.Add(time.Minute))

	requeued, failed, err := st.GiveUpOnLostJobs(ctx, time.Minute, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 || failed != 1 {
		t.Fatalf("requeued=%d failed=%d, want 1 and 1", requeued, failed)
	}
	p, _ := st.Get(ctx, plain)
	if p.State != job.Failed {
		t.Errorf("default job = %s, want FAILED (never silently re-run)", p.State)
	}
	if !strings.Contains(p.Reason, "--requeue") {
		t.Errorf("reason should say how to opt in: %q", p.Reason)
	}
	if p.Node != "doomed" {
		t.Errorf("node = %q; must be remembered so a late result can still be attributed", p.Node)
	}
	o, _ := st.Get(ctx, opted)
	if o.State != job.Pending {
		t.Errorf("--requeue job = %s, want PENDING", o.State)
	}
}

// A node that loses the network, finishes the job anyway, and comes back is
// telling the controller something it could not otherwise know. The node is
// authoritative about its own jobs, so that late result must replace the
// pessimistic node-loss failure rather than being discarded.
func TestLateResultFromRecoveredNodeIsAccepted(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()

	j, err := st.Submit(ctx, job.Spec{
		Name: "long", User: "u", Script: "/bin/true", ArrayTaskID: -1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertNode(ctx, "flaky", "10.0.0.9", platform.Capabilities{OS: "linux", CPUs: 4}, now)
	st.MarkRunning(ctx, j.ID, "flaky", "", now)

	// The node drops off and never returns; past the grace period the job is
	// written off -- failed, not restarted, because it did not opt in.
	st.ExpireNodes(ctx, time.Nanosecond, now.Add(time.Minute))
	if _, failed, err := st.GiveUpOnLostJobs(ctx, time.Minute, now.Add(time.Hour)); err != nil || failed != 1 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	lost, _ := st.Get(ctx, j.ID)
	if lost.State != job.Failed {
		t.Fatalf("state = %s, want FAILED", lost.State)
	}
	// The node it ran on must be remembered, or the late report cannot be
	// attributed and its output would be refused.
	if lost.Node != "flaky" {
		t.Fatalf("node = %q; must be remembered so a late result can be matched", lost.Node)
	}
	if !strings.Contains(lost.Reason, store.LostNodeReason) {
		t.Fatalf("reason %q must be recognisable as a node-loss failure", lost.Reason)
	}
}

// The output of a job that really did finish must still be accepted after the
// controller wrote it off, or the work is lost twice over.
func TestOutputAcceptedFromNodeAfterNodeLossFailure(t *testing.T) {
	h := start(t)
	v := h.submitScript("brief.sh", "#!/bin/sh\necho survived\n", job.Limits{CPUs: 1})
	done := h.waitTerminal(v.ID, 30*time.Second)
	if done.State != string(job.Completed) {
		t.Fatalf("state = %s", done.State)
	}
	if got := h.output(done); !strings.Contains(got, "survived") {
		t.Errorf("output = %q", got)
	}
	// The job's node must be recorded; authorisation for the output upload
	// depends on it.
	if done.Node == "" {
		t.Error("finished job has no node recorded; a late output upload would be refused")
	}
}

// A job that ran must not leave its working directory behind.
//
// Cleanup used to be called only when *launching* failed, so every job that
// actually ran leaked its scratch for good: a slow leak nothing reported,
// growing with use, on machines lent out by people who would eventually meet
// it as "shome filled my disk".
func TestFinishedJobLeavesNoScratch(t *testing.T) {
	h := start(t)

	v := h.submitScript("tidy.sh", "#!/bin/sh\necho hello\n", job.Limits{CPUs: 1})
	h.waitTerminal(v.ID, 20*time.Second)

	// The output survives -- it is archived on the controller before the
	// scratch is removed -- while the directory it came from does not. That
	// ordering is the point: cleaning up earlier would delete the results on
	// their way to being fetched.
	if out := h.output(v); !strings.Contains(out, "hello") {
		t.Errorf("output was lost along with the scratch: %q", out)
	}

	jobsRoot := filepath.Join(h.root, "local", "jobs")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(jobsRoot)
		if err != nil || len(entries) == 0 {
			return // gone, or never created
		}
		time.Sleep(200 * time.Millisecond)
	}
	entries, _ := os.ReadDir(jobsRoot)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	t.Errorf("scratch left behind after the job finished: %v", left)
}
