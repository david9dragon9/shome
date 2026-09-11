package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/pki"
	"github.com/davidwu/shome/internal/xfer"
)

// HeartbeatInterval is how often an agent reports in. Must be comfortably
// under the controller's NodeTimeout so a single dropped request does not mark
// a healthy node DOWN. A var so tests can shorten it.
var HeartbeatInterval = 3 * time.Second

// Client is the agent's connection to the controller.
type Client struct {
	Addr string // host:port
	Node string
	TLS  *tls.Config
	// StatusRoot is where the local status file is published, for the
	// machine's owner to read without a controller.
	StatusRoot string
	http       *http.Client
	// long is for requests that are meant to last: an interactive session's
	// two streams, and a file transfer.
	//
	// The ordinary client has a ten-second timeout, which is right for a
	// heartbeat and wrong for anything that stays open on purpose. Sharing
	// it meant `srun --pty` worked for ten seconds and then stopped
	// accepting input, and a file transfer larger than ten seconds' worth
	// of bandwidth could not finish -- both with no error, because a
	// timeout on a stream that is idle by design looks exactly like the
	// far end having nothing to say.
	long    *http.Client
	backoff time.Duration

	// apiProxy forwards jobs' cluster-API requests to the controller, built
	// once because a reverse proxy holds connection state worth reusing.
	apiOnce  sync.Once
	apiProxy *httputil.ReverseProxy
}

func NewClient(addr, node string, caPEM, certPEM, keyPEM []byte, serverName string) (*Client, error) {
	cfg, err := pki.ClientTLS(caPEM, certPEM, keyPEM, serverName)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{TLSClientConfig: cfg}
	return &Client{
		Addr: addr, Node: node, TLS: cfg,
		http: &http.Client{
			Timeout:   10 * time.Second,
			Transport: tr,
		},
		// No timeout: these end when the work ends, and the context they
		// are given is what cancels them.
		long: &http.Client{Transport: tr},
	}, nil
}

// Join exchanges a single-use token for a signed certificate. It needs no
// client certificate, which is the whole point, but still verifies the
// controller -- against the CA it is about to be handed.
//
// That is trust-on-first-use: the token is the out-of-band secret that makes
// it safe, since only the real controller can validate it.
func Join(ctx context.Context, addr, node, token string) (agentapi.JoinResponse, error) {
	var out agentapi.JoinResponse
	body, _ := json.Marshal(agentapi.JoinRequest{Token: token, Node: node})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+addr+"/agent/join", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	cli := &http.Client{
		Timeout: 15 * time.Second,
		// The CA is unknown until this call returns it; the join token is the
		// authenticator. Subsequent calls verify against the returned CA.
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}},
	}
	resp, err := cli.Do(req)
	if err != nil {
		return out, fmt.Errorf("cannot reach controller at %s: %w", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		return out, fmt.Errorf("join refused by controller: %s", bytes.TrimSpace(buf.Bytes()))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	if out.NodeCert == "" || out.NodeKey == "" || out.CACert == "" {
		return out, fmt.Errorf("controller returned an incomplete identity")
	}
	return out, nil
}

// UploadOutput sends a finished job's captured output to the controller.
//
// Agent-initiated, like everything else: the controller cannot dial an agent
// behind NAT, so results are pushed rather than pulled. Best-effort -- the job
// result itself is already recorded, and a failed upload must not lose it.
func (c *Client) UploadOutput(ctx context.Context, jobID int64, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://%s/agent/output/%d", c.Addr, jobID), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.long.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("output upload rejected: %s", resp.Status)
	}
	return nil
}

// DownloadStageIn fetches a job's input archive from the controller.
func (c *Client) DownloadStageIn(ctx context.Context, jobID int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://%s/agent/stagein/%d", c.Addr, jobID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("stage-in download failed: %s", resp.Status)
	}
	return resp.Body, nil
}

// UploadStageOut returns a job's requested result files to the controller.
func (c *Client) UploadStageOut(ctx context.Context, jobID int64, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://%s/agent/stageout/%d", c.Addr, jobID), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.long.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stage-out upload rejected: %s", resp.Status)
	}
	return nil
}

// localAddr reports the address this agent reaches the controller from.
//
// Determined by opening a UDP socket toward the controller and reading the
// chosen source address -- no packets are sent. This picks the interface the
// machine actually routes over, which is the one peers can reach it on, rather
// than guessing from the interface list.
func (c *Client) localAddr() string {
	conn, err := net.Dial("udp", c.Addr)
	if err != nil {
		return ""
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	return host
}

// Leave tells the controller this node is going away for good, so it stops
// appearing in sinfo as permanently DOWN.
func (c *Client) Leave(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+c.Addr+"/agent/leave", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		return fmt.Errorf("leave refused (%s): %s", resp.Status, bytes.TrimSpace(buf.Bytes()))
	}
	return nil
}

func (c *Client) heartbeat(ctx context.Context, hb agentapi.Heartbeat) (agentapi.HeartbeatResponse, error) {
	var out agentapi.HeartbeatResponse
	body, _ := json.Marshal(hb)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+c.Addr+"/agent/heartbeat", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		return out, fmt.Errorf("heartbeat rejected (%s): %s", resp.Status, bytes.TrimSpace(buf.Bytes()))
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// RunLoop heartbeats until ctx is cancelled, executing whatever the controller
// asks for.
//
// Connection failures are expected, not exceptional: a laptop closes, wifi
// drops, the controller restarts. The loop backs off and keeps going, and jobs
// already running are unaffected because supervision is local.
func (a *Agent) RunLoop(ctx context.Context, c *Client, caps func() agentapi.Heartbeat) {
	backoff := time.Second
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		// Apply the owner's policy before reporting, so the heartbeat
		// reflects this machine's real availability right now.
		od := a.ApplyOwnerPolicy(ctx)

		hb := caps()
		hb.Node = c.Node
		hb.Jobs = a.Statuses()
		hb.Live = a.LiveJobs()
		hb.Claimed, hb.ClaimsOK = a.ClaimedJobs(), true
		var storageStart time.Time
		hb.Storage, storageStart, hb.StorageOK = a.StorageReport()
		if hb.StorageOK {
			hb.StorageAt = storageStart.UnixMilli()
		}
		hb.Transfers = a.TakeTransferResults()
		hb.Addr = c.localAddr()
		hb.OwnerAction = string(od.Action)
		hb.OwnerReason = od.Reason

		hb.Telemetry = a.Telemetry(ctx)
		// A disk limit drains the node rather than bouncing each launch.
		//
		// Refusing at launch time was the first attempt and livelocked: the
		// controller marked the job running, the agent said no, the
		// controller requeued it, and the pair cycled forever on every
		// heartbeat. Reporting it as a drain instead means the scheduler
		// stops offering work here, sinfo says why, and it clears by itself
		// when space is freed -- the same shape the owner's own yield rules
		// already use.
		if why := a.Owner.CheckDisk(diskState(hb.Telemetry)); why != "" {
			hb.OwnerAction, hb.OwnerReason = "drain", why
		}
		// A machine that cannot confine a job does not take cluster work at
		// all. Same mechanism as the disk rule and for the same reason --
		// refusing at launch livelocks -- and it clears by itself once the
		// missing sandbox is installed.
		//
		// This is the one drain an owner cannot override by resuming the
		// node: the others are about what the owner is willing to lend, this
		// one is about whether other people's code can be contained.
		if action, why := a.IsolationDrain(); action != "" {
			hb.OwnerAction, hb.OwnerReason = action, why
		}
		// The machine's real capacity, kept before the owner's caps are
		// applied. The status file reports both, so an owner can see the
		// difference their policy makes -- reporting the capped figure as the
		// total showed "6 of 6" and hid the very thing that panel is for.
		rawCaps := hb.Caps
		// Advertise only what the owner offered.
		hb.Caps = a.CappedCaps(hb.Caps)

		resp, err := c.heartbeat(ctx, hb)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// Publish before backing off. An owner asking what shome is doing
			// to their machine is most likely to ask when the cluster is
			// unreachable, so the local view must survive that -- and must
			// say so rather than showing stale numbers as current.
			a.WriteStatus(c.StatusRoot, a.buildStatus(hb, rawCaps, false, err.Error(), a.Completed()))
			a.Log.Warn("heartbeat failed; retrying", "err", err, "in", backoff)
			t.Reset(backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		a.WriteStatus(c.StatusRoot, a.buildStatus(hb, rawCaps, true, "", a.Completed()))

		// Upload output BEFORE acking: an ack drops our record of the job, so
		// uploading afterwards would lose the path on a crash in between.
		for _, id := range resp.Acked {
			outPath, workDir, stageOut, ok := a.Finished(id)
			if !ok {
				continue
			}
			if f, err := os.Open(outPath); err == nil {
				if err := c.UploadOutput(ctx, id, f); err != nil {
					a.Log.Warn("output upload failed", "job", id, "err", err)
				}
				f.Close()
			}
			if len(stageOut) > 0 {
				var buf bytes.Buffer
				if err := xfer.Tar(&buf, workDir, stageOut); err != nil {
					a.Log.Warn("stage-out pack failed", "job", id, "err", err)
				} else if err := c.UploadStageOut(ctx, id, &buf); err != nil {
					a.Log.Warn("stage-out upload failed", "job", id, "err", err)
				} else {
					a.Log.Info("staged out results", "job", id, "paths", stageOut)
				}
			}
			a.ForgetFinished(id)
		}
		if !resp.Known {
			// This machine was removed from the cluster. Not an error to
			// retry: continuing to heartbeat would be talking to a cluster
			// that has said it does not have us, and the jobs we are
			// holding resources for are not its any more.
			a.Log.Info("this machine has been removed from the cluster; stopping. " +
				"To add it again, run the command from 'shome invite' on the controller")
			return
		}
		a.AckDone(resp.Acked)
		// What this cluster and this machine are called, for the prompt an
		// interactive job gets. Recorded every heartbeat so a rename takes
		// effect within seconds rather than at the next restart.
		a.SetNames(resp.Cluster, resp.Label)

		for _, act := range resp.Actions {
			switch act.Kind {
			case agentapi.ActionLaunch:
				if !a.OwnerAllowsNewWork() {
					// The owner's veto is absolute; the controller will
					// reschedule elsewhere or wait.
					d := a.Owner.Current()
					a.Log.Info("refusing new work: owner policy", "job", act.JobID, "reason", d.Reason)
					a.ReportRefused(act.JobID, "node unavailable: "+d.Reason)
					continue
				}
				if act.Spec == nil {
					a.Log.Error("launch action with no spec", "job", act.JobID)
					continue
				}
				spec := *act.Spec
				var stageIn func(string) error
				if spec.StageIn {
					stageIn = func(workDir string) error {
						body, err := c.DownloadStageIn(ctx, act.JobID)
						if err != nil {
							return err
						}
						defer body.Close()
						return xfer.Untar(body, workDir)
					}
				}
				// Always provide rank wiring, even for one node. Slurm sets
				// SLURM_NNODES=1 for a single-node job, and a script that has
				// to test whether the variable exists is a script that breaks
				// the first time it is run on one machine.
				g := &GangInfo{
					Rank: act.Rank, NNodes: max(act.NNodes, 1),
					NodeList: act.NodeList, MasterAddr: act.MasterAddr,
					MasterPort: act.MasterPort, Fabric: act.Fabric, Role: act.Role,
					FabricEnv: act.FabricEnv, Command: act.Command,
					Pre: act.Pre, Post: act.Post,
				}
				if len(g.NodeList) == 0 {
					g.NodeList = []string{c.Node}
				}
				if g.MasterAddr == "" {
					g.MasterAddr = c.Node
				}
				if g.MasterPort == 0 {
					g.MasterPort = 29500
				}
				if err := a.Launch(ctx, act.JobID, spec, act.Token, g, stageIn); err != nil {
					a.Log.Error("launch failed", "job", act.JobID, "err", err)
					// Report the failure rather than leaving the controller to
					// believe the job is running.
					a.mu.Lock()
					a.done = append(a.done, agentapi.JobStatus{
						ID: act.JobID, State: "FAILED", ExitCode: -1,
						Reason: "launch failed: " + err.Error(),
					})
					a.mu.Unlock()
				}
			case agentapi.ActionKill:
				if err := a.Kill(ctx, act.JobID); err != nil {
					a.Log.Error("kill failed", "job", act.JobID, "err", err)
				}
			case agentapi.ActionFileSend:
				// Serially, not in a goroutine: a burst of copies would
				// otherwise open as many concurrent transfers as the
				// controller queued, and a node lent out by somebody should
				// not saturate their uplink because a user typed a loop.
				a.SendFile(ctx, c, act)
			case agentapi.ActionFileRecv:
				a.RecvFile(ctx, c, act)
			case agentapi.ActionFileRemove:
				a.RemoveFile(act)

			case agentapi.ActionStop:
				a.Log.Info("controller asked this agent to stop")
				// Flush final job reports before going, so results are not
				// lost to the shutdown.
				final := agentapi.Heartbeat{Node: c.Node, Caps: hb.Caps, Jobs: a.Statuses()}
				if _, err := c.heartbeat(ctx, final); err != nil {
					a.Log.Warn("final report failed", "err", err)
				}
				return
			}
		}
		t.Reset(HeartbeatInterval)
	}
}

// UploadUserFile sends one of a user's files to the controller, for relay to
// another machine.
func (c *Client) UploadUserFile(ctx context.Context, transfer int64, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://%s/agent/userfile/%d", c.Addr, transfer), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.long.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		return fmt.Errorf("upload refused (%s): %s", resp.Status, bytes.TrimSpace(buf.Bytes()))
	}
	return nil
}

// DownloadUserFile fetches a relayed file from the controller.
func (c *Client) DownloadUserFile(ctx context.Context, transfer int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://%s/agent/userfile/%d", c.Addr, transfer), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		return nil, fmt.Errorf("download refused (%s): %s", resp.Status, bytes.TrimSpace(buf.Bytes()))
	}
	return resp.Body, nil
}

// The node's half of the interactive relay. Separate requests rather than
// one bidirectional connection, because an agent's HTTP client cannot rely
// on holding a duplex stream open through whatever sits between it and the
// controller.

// StreamOut sends a session's output to the controller as a request body,
// which ends when r does.
func (c *Client) StreamOut(ctx context.Context, stream int64, r io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://%s/agent/stream/%d/out", c.Addr, stream), r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.long.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("output relay refused: %s", resp.Status)
	}
	return nil
}

// StreamIn opens the user's keystrokes as a stream. The caller closes it.
func (c *Client) StreamIn(ctx context.Context, stream int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://%s/agent/stream/%d/in", c.Addr, stream), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("input relay refused: %s", resp.Status)
	}
	return resp.Body, nil
}

// ProxyAPI forwards one of a job's cluster-API requests to the controller.
//
// Over the connection this node already has, because a job cannot reach the
// controller's own API: it listens on a unix socket on the controller's
// machine, which is usually not this one and is denied by the job's sandbox
// even when it is. See internal/ctl/jobapi.go for the whole picture.
//
// The request is forwarded as it arrives, Authorization header included --
// that header is the job's credential and the only thing that decides what
// it may do. This node's certificate gets the request accepted for
// forwarding and nothing more.
func (c *Client) ProxyAPI(w http.ResponseWriter, r *http.Request) {
	c.apiOnce.Do(func() {
		c.apiProxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "https"
				pr.Out.URL.Host = c.Addr
				pr.Out.URL.Path = "/agent/api" + pr.In.URL.Path
				pr.Out.Host = c.Addr
				// The client sent its credential; nothing else about the
				// caller is ours to assert. In particular no X-Forwarded-*
				// headers, which the API does not read and which would
				// invite somebody to start trusting them.
				pr.Out.Header.Del("X-Forwarded-For")
			},
			Transport: c.long.Transport,
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				// The shape the CLI already understands, so a job sees a
				// sentence about the cluster being unreachable rather than
				// a bare gateway error.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				fmt.Fprintf(w, `{"error":%q}`,
					"cannot reach the controller from this node: "+err.Error())
			},
		}
	})
	c.apiProxy.ServeHTTP(w, r)
}

// StreamSize waits for a window size the node has not applied yet.
//
// Returns the size it found and the version that names it; a version equal
// to the one asked for means the wait ended with nothing new, which is the
// ordinary outcome of a session whose window is not being resized.
func (c *Client) StreamSize(ctx context.Context, stream int64, since int64) (job.Winsize, int64, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("https://%s/agent/stream/%d/size?since=%d", c.Addr, stream, since), nil)
	if err != nil {
		return job.Winsize{}, since, err
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return job.Winsize{}, since, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return job.Winsize{}, since, nil
	}
	if resp.StatusCode != http.StatusOK {
		return job.Winsize{}, since, fmt.Errorf("terminal size refused: %s", resp.Status)
	}
	var got struct {
		job.Winsize
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return job.Winsize{}, since, err
	}
	return got.Winsize, got.Version, nil
}

// StreamExit reports the process's exit status, which ends the session.
func (c *Client) StreamExit(ctx context.Context, stream int64, code int) error {
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://%s/agent/stream/%d/exit?code=%d", c.Addr, stream, code), nil)
	if err != nil {
		return err
	}
	resp, err := c.long.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}
