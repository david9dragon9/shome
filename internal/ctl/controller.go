// Package ctl is the shome controller: it owns the job store and the node
// registry, decides what runs where, and hands actions to node agents.
//
// The controller never executes a job itself. Even the agent on the
// controller's own machine goes through the network protocol, so there is one
// code path rather than a well-tested remote path and a special-cased local
// one that silently diverges.
package ctl

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/clustercfg"
	"github.com/davidwu/shome/internal/fabric"
	"github.com/davidwu/shome/internal/fairshare"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/sched"
	"github.com/davidwu/shome/internal/sshca"
	"github.com/davidwu/shome/internal/store"
)

const (
	// Tick is the scheduling interval. Job supervision happens in the agent at
	// a much finer interval; this loop only places work.
	Tick = 250 * time.Millisecond

	// JobGrace is how long a job may keep running on a node we cannot see
	// before we give up on it.
	//
	// Deliberately much longer than NodeTimeout, because they answer different
	// questions. NodeTimeout asks "should we send this machine more work?" --
	// no, immediately. JobGrace asks "is the work already running there
	// lost?" -- probably not; the agent does not kill jobs when it loses the
	// controller, so a laptop on spotty Wi-Fi is still computing. Waiting is
	// nearly free, and being wrong here destroys real work.
	JobGrace = 10 * time.Minute

	// NodeTimeout is how long a node may go unheard-of before it stops being
	// given new work. It does NOT affect jobs already running there.
	NodeTimeout = 30 * time.Second
)

type Controller struct {
	store         *store.Store
	log           *slog.Logger
	root          string // state dir; job output is archived under root/output
	stor          *Storage
	userCA        *sshca.CA
	loginAccounts []string

	mu sync.Mutex
	// pending holds actions queued for each node until its next heartbeat.
	pending map[string][]agentapi.Action

	// metrics is live utilisation from every node. In memory only; see
	// metrics.go for why it is never persisted.
	metrics *Metrics

	// qos holds the cluster-wide limits, reloaded when the file changes.
	qos *qosCache

	// xfer tracks in-flight file movements between machines.
	xfer *transfers

	// streams carries interactive sessions between a user's terminal and a
	// process on a compute node.
	streams *streams

	// priority is the fair-share policy, reloaded when priority.yaml
	// changes. Disabled by default, in which case the queue is submission
	// order exactly as it was before this existed.
	priority *fairshare.Cache

	// shares caches the computed standings for a few seconds, and scores
	// holds the last pass's per-job priorities for display.
	shares  shareCache
	scoreMu sync.Mutex
	scores  map[int64]fairshare.Score

	// lastReclaim is when each node's records were last reconciled against
	// what it says it is running. See reclaimUnclaimed.
	lastReclaim map[string]time.Time

	// noteAt is when the controller last learned of a file on a machine
	// from a transfer rather than from that machine's own report, per
	// node and account. See notedSince.
	noteAt map[string]time.Time

	// login is where an outside user reaches this cluster, recorded by the
	// daemon at startup so nothing has to guess it locally.
	login loginState

	// cluster holds the cluster's name and the admin's labels for machines,
	// reloaded when cluster.yaml changes so hand-editing needs no restart.
	cluster *clustercfg.Cache

	// onRevoke, when set, is told that a user's access has been withdrawn so
	// open sessions can be closed at once rather than at the next login. A
	// hook rather than a direct dependency: the controller must not need to
	// know that a login node exists, and works fine when none does.
	onRevoke func(user, fingerprint string)

	nowFn    func() time.Time
	stopOnce sync.Once
	stopCh   chan struct{}
}

func New(st *store.Store, root string, log *slog.Logger) *Controller {
	return &Controller{
		store: st, root: root, stor: NewStorage(root), log: log,
		pending:     map[string][]agentapi.Action{},
		lastReclaim: map[string]time.Time{},
		noteAt:      map[string]time.Time{},
		metrics:     NewMetrics(),
		qos:         newQoSCache(root),
		xfer:        newTransfers(),
		cluster:     clustercfg.NewCache(root),
		priority:    fairshare.NewCache(root),
		streams:     newStreams(),
		nowFn:       time.Now, stopCh: make(chan struct{}),
	}
}

func (c *Controller) now() time.Time { return c.nowFn() }

// SetRevocationHook registers a callback invoked when a user's access is
// withdrawn, so live sessions can be ended immediately rather than at the next
// login attempt.
func (c *Controller) SetRevocationHook(f func(user, fingerprint string)) { c.onRevoke = f }

// Metrics is live per-node utilisation.
func (c *Controller) Metrics() *Metrics   { return c.metrics }
func (c *Controller) Store() *store.Store { return c.store }
func (c *Controller) Storage() *Storage   { return c.stor }

// ClusterConfig returns the cluster's name and machine labels.
func (c *Controller) ClusterConfig() clustercfg.Config { return c.cluster.Get(c.log) }

// ClusterName is what this cluster is called.
func (c *Controller) ClusterName() string { return c.ClusterConfig().ClusterName() }

// LabelOf is what to call a machine in anything a person reads.
func (c *Controller) LabelOf(identity string) string { return c.ClusterConfig().LabelOf(identity) }

// ResolveNode turns a name someone typed -- an identity or an admin's label
// -- into the machine's certificate identity.
//
// Every path that takes a node name from a user goes through this, so that
// labelling a machine "mini" makes "mini" work in 'shome fs', 'scontrol' and
// the console, rather than only in listings.
func (c *Controller) ResolveNode(name string) string { return c.ClusterConfig().Resolve(name) }

// NodeIdentity turns a name a person typed into a machine that exists.
//
// One choke point for both halves -- resolving an admin's label and checking
// the machine is real -- because they belong together: resolving without
// checking accepts a typo as a literal name, and checking without resolving
// rejects the very label the admin chose. Doing it per call site is how one
// of the two gets forgotten.
func (c *Controller) NodeIdentity(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("no machine named")
	}
	id := c.ResolveNode(name)
	if _, err := c.store.NodeByName(ctx, id); err != nil {
		known, _ := c.store.Nodes(ctx)
		if len(known) == 0 {
			return "", fmt.Errorf("no machine called %q -- this cluster has no machines yet", name)
		}
		names := make([]string, 0, len(known))
		cfg := c.ClusterConfig()
		for _, n := range known {
			names = append(names, cfg.LabelOf(n.Name))
		}
		sort.Strings(names)
		return "", fmt.Errorf("no machine called %q in this cluster. Known: %s",
			name, strings.Join(names, ", "))
	}
	return id, nil
}

// SetClusterConfig replaces cluster.yaml.
func (c *Controller) SetClusterConfig(cfg clustercfg.Config) error {
	return clustercfg.Save(c.root, cfg)
}

// LoginAccounts are the OS account names the login node's sshd will accept.
// Every certificate includes them as principals, because a shome user has no
// OS account of their own to log in as.
func (c *Controller) LoginAccounts() []string { return c.loginAccounts }

func (c *Controller) SetLoginAccounts(a []string) { c.loginAccounts = a }

// UserCA is the SSH CA that signs user certificates; nil if SSH is disabled.
func (c *Controller) UserCA() *sshca.CA      { return c.userCA }
func (c *Controller) SetUserCA(ca *sshca.CA) { c.userCA = ca }

// RequestStop asks Run to return, so `shome shutdown` needs no signal.
func (c *Controller) RequestStop() { c.stopOnce.Do(func() { close(c.stopCh) }) }

func (c *Controller) Run(ctx context.Context) error {
	// Nothing is decided about running jobs here, deliberately.
	//
	// A controller that has just started cannot tell a crash from a planned
	// restart, and in neither case does it know what the machines are doing:
	// an agent keeps supervising its jobs when it loses the controller, so
	// most of what the records call RUNNING really is. Failing them all --
	// which is what this used to do -- destroyed live work on every
	// `shome restart`, and overrode the deliberate decision to wait out an
	// unreachable node.
	//
	// The machines answer the question themselves within a heartbeat. See
	// reclaimUnclaimed.

	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopCh:
			return nil
		case <-t.C:
			c.expireNodes(ctx)
			c.reconcileServices(ctx)
			c.schedule(ctx)
			c.reapJobTokens(ctx)
			c.reapOrphanStreams(ctx)
		}
	}
}

// expireNodes marks silent nodes DOWN and requeues their work.
func (c *Controller) expireNodes(ctx context.Context) {
	gone, err := c.store.ExpireNodes(ctx, NodeTimeout, c.now())
	if err != nil {
		c.log.Error("expire nodes", "err", err)
		return
	}
	for _, n := range gone {
		// The node stops receiving work immediately, but its running jobs are
		// left alone: the agent keeps computing when it loses us, so they are
		// most likely fine. They are only annotated so squeue is honest about
		// what we can and cannot see.
		marked, err := c.store.MarkJobsUnreachable(ctx, n)
		if err != nil {
			c.log.Error("annotating jobs on unreachable node", "node", n, "err", err)
		}
		c.log.Warn("node unreachable; its jobs are left running",
			"node", n, "jobs", marked, "give_up_after", JobGrace)
		c.store.Event(ctx, 0, "node_down", n, c.now())
		c.mu.Lock()
		delete(c.pending, n)
		c.mu.Unlock()
	}

	// Only after a much longer grace do we conclude the work is really gone.
	requeued, failed, err := c.store.GiveUpOnLostJobs(ctx, JobGrace, c.now())
	if err != nil {
		c.log.Error("giving up on lost jobs", "err", err)
	} else if requeued+failed > 0 {
		c.log.Warn("gave up on jobs whose node never returned",
			"requeued", requeued, "failed", failed, "after", JobGrace)
	}
}

// nodeStates builds the scheduler's view from the registry plus the jobs
// currently assigned to each node.
func (c *Controller) NodeStates(ctx context.Context) ([]sched.NodeState, error) {
	nodes, err := c.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	active, err := c.store.List(ctx, "", true)
	if err != nil {
		return nil, err
	}
	cfg := c.ClusterConfig()
	used := map[string]*sched.NodeState{}
	for _, n := range nodes {
		if n.State == store.NodeDown {
			continue // not a candidate at all
		}
		used[n.Name] = &sched.NodeState{
			// Name is the certificate identity, which is what placement and
			// every record key off. Label is only ever displayed -- a
			// scheduler reason naming a machine should name it the way the
			// admin did, not the way its certificate does.
			Name: n.Name, Label: cfg.LabelOf(n.Name),
			CPUs: n.Caps.CPUs, MemBytes: n.Caps.MemBytes,
			GPUs: n.Caps.GPUs, GPUMemBytes: n.Caps.GPUMemBytes,
			Drained: n.State == store.NodeDrain,
			// DOWN nodes were already skipped above, so anything reaching
			// here is a live candidate. Without this the aggregate solver
			// treats every node as offline and refuses every request.
			Online: true,
			Features: sched.NodeFeatures{
				OS: n.Caps.OS, Arch: n.Caps.Arch, GPUKind: n.Caps.GPUKind,
				Tier: n.Caps.Tier, CPUs: n.Caps.CPUs, MemBytes: n.Caps.MemBytes,
				GPUs: n.Caps.GPUs, GPUMem: n.Caps.GPUMemBytes,
			},
		}
	}
	for _, j := range active {
		if j.State != job.Running || j.Node == "" {
			continue
		}
		ns := used[j.Node]
		if ns == nil {
			continue
		}
		cpus := j.Spec.Limits.CPUs
		if cpus <= 0 {
			cpus = 1
		}
		ns.UsedCPUs += cpus
		ns.UsedMem += j.Spec.Limits.MemBytes
		ns.UsedGPUs += j.Spec.Limits.GPUs
	}
	out := make([]sched.NodeState, 0, len(used))
	for _, n := range nodes {
		if ns, ok := used[n.Name]; ok {
			out = append(out, *ns)
		}
	}
	return out, nil
}

// reconcileServices keeps each running service backed by a live job.
//
// Restart-with-backoff rather than immediate resubmission: a service whose
// command exits instantly would otherwise spin, filling the queue and the
// audit log faster than anyone could read them.
func (c *Controller) reconcileServices(ctx context.Context) {
	svcs, err := c.store.Services(ctx, "")
	if err != nil {
		return
	}
	for _, svc := range svcs {
		if svc.Desired != "running" {
			continue
		}
		if svc.JobID != 0 {
			j, err := c.store.Get(ctx, svc.JobID)
			if err == nil && !j.State.Terminal() {
				continue // still alive
			}
		}
		// Back off after repeated restarts so a crash-looping service does
		// not saturate the scheduler.
		if !svc.LastStart.IsZero() {
			wait := time.Duration(min(svc.Restarts, 6)) * 10 * time.Second
			if c.now().Sub(svc.LastStart) < wait {
				continue
			}
		}
		spec := svc.Spec
		spec.User = svc.Owner
		spec.Name = "svc:" + svc.Name
		spec.ArrayTaskID = -1
		j, err := c.store.Submit(ctx, spec, c.now())
		if err != nil {
			c.log.Error("service submit", "service", svc.Name, "err", err)
			continue
		}
		if err := c.store.SetServiceJob(ctx, svc.Name, j.ID, c.now()); err != nil {
			c.log.Error("service record", "service", svc.Name, "err", err)
			continue
		}
		c.store.Event(ctx, j.ID, "service_start", svc.Name, c.now())
		c.log.Info("service (re)started", "service", svc.Name, "job", j.ID,
			"restarts", svc.Restarts+1)
	}
}

func (c *Controller) schedule(ctx context.Context) {
	pending, err := c.store.Pending(ctx)
	if err != nil || len(pending) == 0 {
		return
	}
	nodes, err := c.NodeStates(ctx)
	if err != nil {
		c.log.Error("node states", "err", err)
		return
	}
	if len(nodes) == 0 {
		for _, j := range pending {
			c.store.SetReason(ctx, j.ID, "no nodes registered")
		}
		return
	}

	// Dependencies are resolved before placement: a job whose prerequisites
	// are unmet is not a candidate at all, and must not block the queue.
	runnable := make([]*job.Job, 0, len(pending))
	for _, j := range pending {
		ok, reason, never, err := c.depsSatisfiedEx(ctx, j)
		if err != nil {
			c.log.Error("dependency check", "job", j.ID, "err", err)
			continue
		}
		if never {
			// It can never run. Cancelling is kinder than leaving it queued
			// forever pretending otherwise.
			if reason == "" {
				reason = "dependency can never be satisfied"
			}
			c.log.Info("cancelling job with unsatisfiable dependency", "job", j.ID, "reason", reason)
			c.store.MarkFinished(ctx, j.ID, job.Cancelled, -1,
				"dependency never satisfied: "+reason, 0, c.now())
			continue
		}
		if !ok {
			if reason == "" {
				reason = "waiting on dependency"
			}
			c.store.SetReason(ctx, j.ID, reason)
			continue
		}
		runnable = append(runnable, j)
	}

	// Hold back work whose owner is already at a concurrency limit. Done
	// before placement so a job over the line stays queued with a reason,
	// rather than being placed and then killed -- and so it starts on its own
	// as soon as their other work finishes.
	runnable = c.admitForQoS(ctx, runnable)

	// Aggregate ("--total-*") jobs are gang-scheduled: all nodes or none.
	// They are handled first and removed from the FIFO list, because a partial
	// allocation is useless -- a distributed run cannot start with three of
	// the four ranks it needs.
	var single []*job.Job
	for _, j := range runnable {
		req := sched.AggregateFromSpec(j.Spec)
		if req.Empty() {
			single = append(single, j)
			continue
		}
		p, why := sched.SolveAggregate(req, nodes)
		if p == nil {
			c.store.SetReason(ctx, j.ID, why)
			continue
		}
		if err := c.launchGang(ctx, j, p, nodes); err != nil {
			c.log.Error("gang launch", "job", j.ID, "err", err)
			c.store.SetReason(ctx, j.ID, "launch failed: "+err.Error())
			continue
		}
		// Reserve the allocation so the next job in this same pass sees the
		// capacity as taken; otherwise two gangs would both be told yes.
		nodes = reserve(nodes, p)
	}
	runnable = single

	// Order the queue. Off by default, in which case this returns the jobs
	// exactly as they came -- submission order.
	//
	// After QoS admission and after gang scheduling, deliberately: admission
	// decides who is eligible at all, and reordering the ineligible would be
	// wasted work. Aggregate jobs are placed by their own solver and never
	// reach the single-node queue.
	runnable = c.orderByPriority(ctx, runnable)

	// What is running where, so backfill can work out when a gap opens.
	if opts := c.schedOptions(); opts.Backfill {
		live := c.runningOn(ctx)
		for i := range nodes {
			nodes[i].Running = live[nodes[i].Name]
		}
	}

	for _, d := range sched.PlanWith(runnable, nodes, c.schedOptions()) {
		if !d.Scheduled() {
			c.store.SetReason(ctx, d.JobID, d.Reason)
			continue
		}
		var j *job.Job
		for _, p := range runnable {
			if p.ID == d.JobID {
				j = p
				break
			}
		}
		if j == nil {
			continue
		}
		if err := c.store.MarkRunning(ctx, j.ID, d.Node, "", c.now()); err != nil {
			c.log.Error("mark running", "job", j.ID, "err", err)
			continue
		}
		c.enqueue(d.Node, agentapi.Action{Kind: agentapi.ActionLaunch, JobID: j.ID,
			Spec: &j.Spec, Token: c.jobToken(ctx, j)})
		c.log.Info("job assigned", "job", j.ID, "node", d.Node)
	}
}

// fabricAlloc converts a placement into the view the fabric layer needs.
func (c *Controller) fabricAlloc(ctx context.Context, jobID int64, names []string, master string, port int) fabric.Allocation {
	a := fabric.Allocation{JobID: jobID, MasterAddr: master, MasterPort: port}
	nodes, err := c.store.Nodes(ctx)
	byName := map[string]*store.Node{}
	if err == nil {
		for _, n := range nodes {
			byName[n.Name] = n
		}
	}
	for _, name := range names {
		fn := fabric.Node{Name: name}
		if n := byName[name]; n != nil {
			fn.Addr = n.Addr
			fn.CPUs = n.Caps.CPUs
			fn.GPUs = n.Caps.GPUs
			fn.GPUKind = n.Caps.GPUKind
			fn.GPUMemBytes = n.Caps.GPUMemBytes
			fn.OS = n.Caps.OS
			fn.Arch = n.Caps.Arch
			fn.OSVersion = n.Caps.OSVersion
		}
		a.Nodes = append(a.Nodes, fn)
	}
	return a
}

// FabricAllocFor builds a fabric view of a set of node names, for planning.
func (c *Controller) FabricAllocFor(ctx context.Context, names []string) fabric.Allocation {
	master := ""
	if len(names) > 0 {
		master = names[0]
	}
	return c.fabricAlloc(ctx, 0, names, master, gangMasterPort)
}

// FabricWorkloadFor is the exported form used by the plan endpoint.
func FabricWorkloadFor(spec job.Spec) fabric.Workload { return fabricWorkload(spec) }

// fabricWorkload describes a job to the fabric selector.
func fabricWorkload(spec job.Spec) fabric.Workload {
	w := fabric.Workload{Requested: spec.Fabric, Model: spec.Model}
	if spec.Model != "" {
		w.Collective = true
	}
	// A Python entry point is what makes Ray applicable.
	script := strings.ToLower(spec.Script)
	if strings.HasSuffix(script, ".py") {
		w.Python = true
		w.Collective = true
	}
	return w
}

// launchGang allocates every node atomically and dispatches one ranked task
// to each.
func (c *Controller) launchGang(ctx context.Context, j *job.Job, p *sched.Placement, nodes []sched.NodeState) error {
	names := p.NodeNames()
	if err := c.store.CreateTasks(ctx, j.ID, names, c.now()); err != nil {
		return err
	}
	// Rank 0 is the rendezvous point for fabrics that need one.
	master := names[0]
	addr := master
	for _, n := range nodes {
		if n.Name == master && n.Addr != "" {
			addr = n.Addr
			break
		}
	}
	if err := c.store.MarkRunning(ctx, j.ID, strings.Join(names, ","), "", c.now()); err != nil {
		c.store.ClearTasks(ctx, j.ID)
		return err
	}
	// Pick how the ranks coordinate. Failing to plan is not fatal: the job can
	// still run as independent ranks, which is what most work wants anyway.
	fa := c.fabricAlloc(ctx, j.ID, names, addr, gangMasterPort)
	fw := fabricWorkload(j.Spec)
	var rankPlans []fabric.RankPlan
	fabricName := "none"
	choice, err := fabric.Select(fa, fw)
	if err != nil {
		if j.Spec.Fabric != "" {
			// An explicit request that cannot be honoured must fail loudly
			// rather than silently running something else.
			c.store.ClearTasks(ctx, j.ID)
			return fmt.Errorf("fabric %q: %w", j.Spec.Fabric, err)
		}
		c.log.Warn("no fabric selected; running as independent ranks", "job", j.ID, "err", err)
	} else {
		fabricName = choice.Fabric.Name()
		rankPlans, err = choice.Fabric.Plan(fa, fw)
		if err != nil {
			c.store.ClearTasks(ctx, j.ID)
			return fmt.Errorf("fabric %s: %w", fabricName, err)
		}
		c.log.Info("fabric selected", "job", j.ID, "fabric", fabricName,
			"why", strings.Join(choice.Reasons, "; "))
	}

	// One credential for the whole job, not one per rank: the ranks are one
	// job to the cluster, and `squeue` from any of them is the same request.
	token := c.jobToken(ctx, j)
	for rank, n := range names {
		act := agentapi.Action{
			Kind: agentapi.ActionLaunch, JobID: j.ID, Spec: &j.Spec,
			Token: token,
			Rank:  rank, NNodes: len(names), NodeList: names,
			MasterAddr: addr, MasterPort: gangMasterPort, Fabric: fabricName,
		}
		if rank < len(rankPlans) {
			rp := rankPlans[rank]
			act.Role, act.FabricEnv = string(rp.Role), rp.Env
			act.Command, act.Pre, act.Post = rp.Command, rp.Pre, rp.Post
		}
		c.enqueue(n, act)
	}
	c.log.Info("gang job assigned", "job", j.ID, "nodes", names, "fabric", fabricName, "why", p.Why)
	return nil
}

// gangMasterPort is the rendezvous port distributed runtimes are pointed at.
const gangMasterPort = 29500

// reserve deducts a placement from the node states so later decisions in the
// same scheduling pass see the capacity as committed.
func reserve(nodes []sched.NodeState, p *sched.Placement) []sched.NodeState {
	out := make([]sched.NodeState, len(nodes))
	copy(out, nodes)
	for _, share := range p.Nodes {
		for i := range out {
			if out[i].Name != share.Node {
				continue
			}
			out[i].UsedCPUs += share.CPUs
			out[i].UsedMem += share.MemBytes
			out[i].UsedGPUs += share.GPUs
			out[i].UsedGPUMem += share.GPUMem
		}
	}
	return out
}

func (c *Controller) enqueue(node string, a agentapi.Action) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[node] = append(c.pending[node], a)
}

// HandleHeartbeat records a node's state and job reports and returns its work.
func (c *Controller) HandleHeartbeat(ctx context.Context, hb agentapi.Heartbeat) (agentapi.HeartbeatResponse, []int64, error) {
	now := c.now()
	// A machine somebody removed. Not an error and not silence: it is told
	// it is no longer part of this cluster, which is what stops it. Nothing
	// else is recorded for it -- upserting here is what used to bring a
	// forgotten node back within one heartbeat.
	if gone, err := c.store.NodeForgotten(ctx, hb.Node); err == nil && gone {
		c.log.Info("refusing a heartbeat from a machine that was removed",
			"node", hb.Node)
		return agentapi.HeartbeatResponse{Known: false}, nil, nil
	}
	// The owner's disposition is authoritative. A cluster admin can drain a
	// node, but cannot un-drain one whose owner has taken it back.
	switch hb.OwnerAction {
	case "suspend", "drain":
		c.store.SetNodeState(ctx, hb.Node, store.NodeDrain, "owner: "+hb.OwnerReason)
	case "run", "throttle":
		if n, err := c.store.NodeByName(ctx, hb.Node); err == nil &&
			n.State == store.NodeDrain && strings.HasPrefix(n.Reason, "owner: ") {
			// Only lift a drain this node's owner imposed; an admin drain stays.
			c.store.SetNodeState(ctx, hb.Node, store.NodeUp, "")
		}
	}
	if err := c.store.UpsertNode(ctx, hb.Node, hb.Addr, hb.Caps, now); err != nil {
		return agentapi.HeartbeatResponse{}, nil, err
	}
	// It is back: its jobs were never in trouble, so drop the annotation.
	c.store.ClearUnreachable(ctx, hb.Node)

	c.metrics.Record(hb.Node, hb.Telemetry, hb.Live)
	c.applyStorageReport(ctx, hb)
	c.ApplyTransferResults(ctx, hb.Node, hb.Transfers)

	// Acknowledge terminal reports only after they are durably recorded, so a
	// crash between the two resends rather than loses the result.
	var acked []int64
	for _, s := range hb.Jobs {
		if !s.State.Terminal() {
			if s.PeakMem > 0 {
				c.store.RecordPeak(ctx, s.ID, s.PeakMem)
			}
			continue
		}
		j, err := c.store.Get(ctx, s.ID)
		if err != nil {
			acked = append(acked, s.ID) // unknown job: stop resending
			continue
		}
		if j.State.Terminal() {
			// One exception: a job we failed only because its node vanished.
			// If that node comes back and reports what actually happened, it
			// is telling us something we could not know -- the work may well
			// have finished fine while we were unable to see it. The node
			// that ran the job is authoritative about its outcome.
			if j.Node == hb.Node && strings.Contains(j.Reason, store.LostNodeReason) {
				c.log.Info("correcting a node-loss failure with the node's own result",
					"job", s.ID, "node", hb.Node, "actual", s.State)
				if err := c.store.MarkFinished(ctx, s.ID, s.State, s.ExitCode,
					"completed on "+hb.Node+" during a network outage; result recovered on reconnect",
					s.PeakMem, now); err != nil {
					c.log.Error("record recovered result", "job", s.ID, "err", err)
					continue // do not ack; try again next heartbeat
				}
				c.store.Event(ctx, s.ID, "result_recovered", hb.Node, now)
			}
			acked = append(acked, s.ID) // already recorded
			continue
		}

		// A node that refused the work has not failed it: put the job back in
		// the queue so another machine can take it.
		if s.Refused {
			c.log.Info("node refused work; requeueing", "job", s.ID, "node", hb.Node, "reason", s.Reason)
			c.store.ClearTasks(ctx, s.ID)
			if err := c.store.RequeueJob(ctx, s.ID, s.Reason); err != nil {
				c.log.Error("requeue refused job", "job", s.ID, "err", err)
				continue
			}
			acked = append(acked, s.ID)
			continue
		}

		// A gang job finishes only when every rank has. Record this rank, then
		// see whether the job as a whole is done.
		tasks, terr := c.store.TasksFor(ctx, s.ID)
		if terr == nil && len(tasks) > 0 {
			if err := c.store.FinishTask(ctx, s.ID, hb.Node, s.State, s.ExitCode, s.Reason, now); err != nil {
				// Already recorded, or not this node's task: ack anyway so the
				// agent stops resending a report we cannot place.
				c.log.Warn("record task", "job", s.ID, "node", hb.Node, "err", err)
				acked = append(acked, s.ID)
				continue
			}
			done, st, code, reason, err := c.store.GangOutcome(ctx, s.ID)
			if err != nil {
				c.log.Error("gang outcome", "job", s.ID, "err", err)
				continue
			}
			acked = append(acked, s.ID)
			if !done {
				continue // other ranks still running
			}
			if err := c.store.MarkFinished(ctx, s.ID, st, code, reason, s.PeakMem, now); err != nil {
				c.log.Error("record finish", "job", s.ID, "err", err)
			}
			// One rank failing makes the rest pointless; stop them.
			if st != job.Completed {
				c.stopRemainingRanks(ctx, s.ID, tasks)
			}
			continue
		}

		if err := c.store.MarkFinished(ctx, s.ID, s.State, s.ExitCode, s.Reason, s.PeakMem, now); err != nil {
			c.log.Error("record finish", "job", s.ID, "err", err)
			continue // do not ack; the agent will resend
		}
		acked = append(acked, s.ID)
	}

	// Anything this node's records say it is running, that the node itself
	// does not claim, has ceased to exist.
	c.reclaimUnclaimed(ctx, hb, now)

	c.mu.Lock()
	actions := c.pending[hb.Node]
	delete(c.pending, hb.Node)
	c.mu.Unlock()

	cfg := c.ClusterConfig()
	return agentapi.HeartbeatResponse{
		Actions: actions, Known: true,
		Cluster: cfg.ClusterName(), Label: cfg.LabelOf(hb.Node),
	}, acked, nil
}

// ClaimGrace is how long a job may be recorded as running on a node before
// that node is expected to claim it.
//
// A job is marked RUNNING when it is assigned, which is before the agent has
// seen the action -- so for the first moments after placement the node
// legitimately does not know about it. The window only has to cover an
// action's round trip; it is generous because the cost of being early is
// killing work that was about to start, and the cost of being late is a few
// seconds of a stale queue entry.
const ClaimGrace = 30 * time.Second

// reclaimUnclaimed resolves jobs the node's own report says are not there.
//
// This is how an agent restart is noticed. An agent that comes back -- after
// a reboot, a crash, or `shome restart` -- has no memory of what it was
// running, while the controller's records still say RUNNING. Nothing
// supervises those jobs any more: no memory limit is enforced, no time limit,
// and no result will ever arrive, so they would sit RUNNING for as long as
// the machine kept heartbeating.
//
// Decided from the node's own report rather than guessed at startup. The
// controller used to fail every RUNNING job when it started, on the theory
// that it must have crashed -- which also failed jobs on machines that were
// running them perfectly well, and jobs on a machine that was merely
// unreachable, undoing the deliberate JobGrace policy of waiting for it. A
// node that still has the job says so, and keeps it.
func (c *Controller) reclaimUnclaimed(ctx context.Context, hb agentapi.Heartbeat, now time.Time) {
	if !hb.ClaimsOK {
		return // an agent that cannot say; assume nothing
	}
	// Not on every heartbeat. Those arrive ten times a second per node,
	// while nothing here is urgent -- a job is not acted on until it has
	// been unclaimed for ClaimGrace anyway -- and each pass reads and
	// decodes every running job's spec.
	if !c.dueForReclaim(hb.Node, now) {
		return
	}
	claimed := make(map[int64]bool, len(hb.Claimed)+len(hb.Jobs))
	for _, id := range hb.Claimed {
		claimed[id] = true
	}
	// Also anything reported in this same heartbeat: a result in flight is
	// not a lost job.
	for _, s := range hb.Jobs {
		claimed[s.ID] = true
	}
	running, err := c.store.RunningOn(ctx, hb.Node)
	if err != nil {
		c.log.Error("list running jobs for a node", "node", hb.Node, "err", err)
		return
	}
	for _, j := range running {
		if claimed[j.ID] {
			continue
		}
		if j.StartAt.IsZero() || now.Sub(j.StartAt) < ClaimGrace {
			continue // too soon to expect the node to know about it
		}
		reason := hb.Node + " no longer has this job (its agent restarted)"
		if j.Spec.Requeue {
			c.log.Warn("job lost with its agent; requeueing", "job", j.ID, "node", hb.Node)
			if err := c.store.ClearTasks(ctx, j.ID); err != nil {
				c.log.Error("clear tasks", "job", j.ID, "err", err)
			}
			if err := c.store.RequeueJob(ctx, j.ID, "requeued: "+reason); err != nil {
				c.log.Error("requeue lost job", "job", j.ID, "err", err)
				continue
			}
			c.store.Event(ctx, j.ID, "job_requeued", hb.Node, now)
			continue
		}
		c.log.Warn("job lost with its agent", "job", j.ID, "node", hb.Node)
		if err := c.store.MarkFinished(ctx, j.ID, job.Failed, -1,
			reason+". Submit with --requeue to have it re-run automatically.",
			0, now); err != nil {
			c.log.Error("fail lost job", "job", j.ID, "err", err)
			continue
		}
		c.store.Event(ctx, j.ID, "job_lost", hb.Node, now)
	}
}

// reclaimInterval is how often one node's records are reconciled.
const reclaimInterval = 5 * time.Second

// dueForReclaim rate-limits reconciliation per node, and records the pass.
func (c *Controller) dueForReclaim(node string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.lastReclaim[node]; ok && now.Sub(last) < reclaimInterval {
		return false
	}
	c.lastReclaim[node] = now
	return true
}

// stopRemainingRanks cancels ranks still running after the job has failed.
func (c *Controller) stopRemainingRanks(ctx context.Context, jobID int64, tasks []store.Task) {
	for _, t := range tasks {
		if t.State.Terminal() {
			continue
		}
		c.enqueue(t.Node, agentapi.Action{Kind: agentapi.ActionKill, JobID: jobID})
	}
}

// depsSatisfied evaluates a job's --dependency expression.
// depsSatisfied reports whether a job's dependencies are met.
//
// The third result says the dependency can NEVER be met -- an afterok whose
// prerequisite failed, say. Such a job would otherwise sit PENDING forever,
// accumulating in the queue and misleading anyone reading squeue into thinking
// it is still going to run.
func (c *Controller) depsSatisfiedEx(ctx context.Context, j *job.Job) (ok bool, reason string, never bool, err error) {
	okv, reasonv, errv := c.depsSatisfied(ctx, j)
	if errv != nil || okv {
		return okv, reasonv, false, errv
	}
	deps, perr := ParseDependency(j.Spec.Dependency)
	if perr != nil {
		return false, reasonv, true, nil // a malformed expression never becomes valid
	}
	for _, d := range deps {
		dep, gerr := c.store.Get(ctx, d.JobID)
		if gerr != nil {
			return false, reasonv, true, nil // the prerequisite does not exist
		}
		if !dep.State.Terminal() {
			continue // still could go either way
		}
		switch d.Type {
		case DepAfterOK:
			if dep.State != job.Completed {
				return false, reasonv, true, nil
			}
		case DepAfterNotOK:
			if dep.State == job.Completed {
				return false, reasonv, true, nil
			}
		}
	}
	return false, reasonv, false, nil
}

func (c *Controller) depsSatisfied(ctx context.Context, j *job.Job) (bool, string, error) {
	if j.Spec.Dependency == "" {
		return true, "", nil
	}
	deps, err := ParseDependency(j.Spec.Dependency)
	if err != nil {
		return false, "invalid dependency: " + err.Error(), nil
	}
	for _, d := range deps {
		dep, err := c.store.Get(ctx, d.JobID)
		if err != nil {
			return false, fmt.Sprintf("dependency job %d does not exist", d.JobID), nil
		}
		switch d.Type {
		case DepAfterOK:
			if !dep.State.Terminal() {
				return false, fmt.Sprintf("waiting for job %d to finish", d.JobID), nil
			}
			if dep.State != job.Completed {
				return false, fmt.Sprintf("dependency job %d did not succeed (%s)", d.JobID, dep.State), nil
			}
		case DepAfterAny:
			if !dep.State.Terminal() {
				return false, fmt.Sprintf("waiting for job %d to finish", d.JobID), nil
			}
		case DepAfterNotOK:
			if !dep.State.Terminal() {
				return false, fmt.Sprintf("waiting for job %d to finish", d.JobID), nil
			}
			if dep.State == job.Completed {
				return false, fmt.Sprintf("dependency job %d succeeded, but afternotok was required", d.JobID), nil
			}
		}
	}
	return true, "", nil
}

// Cancel stops a job wherever it is.
func (c *Controller) Cancel(ctx context.Context, id int64) error {
	// A gang job runs on several machines; cancelling must reach all of them.
	if tasks, err := c.store.TasksFor(ctx, id); err == nil && len(tasks) > 0 {
		nodes, err := c.store.CancelTasks(ctx, id, "cancelled by user", c.now())
		if err != nil {
			return err
		}
		for _, n := range nodes {
			c.enqueue(n, agentapi.Action{Kind: agentapi.ActionKill, JobID: id})
		}
		return c.store.MarkFinished(ctx, id, job.Cancelled, -1, "cancelled by user", 0, c.now())
	}
	j, err := c.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if j.State.Terminal() {
		return fmt.Errorf("job %d is already %s", id, j.State)
	}
	if j.State == job.Running && j.Node != "" {
		c.enqueue(j.Node, agentapi.Action{Kind: agentapi.ActionKill, JobID: id})
	}
	return c.store.MarkFinished(ctx, id, job.Cancelled, -1, "cancelled by user", j.PeakMem, c.now())
}

// Hold prevents a pending job from being scheduled until released.
func (c *Controller) Hold(ctx context.Context, id int64) error {
	j, err := c.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if j.State != job.Pending {
		return fmt.Errorf("job %d is %s; only PENDING jobs can be held", id, j.State)
	}
	return c.store.SetHeld(ctx, id, true, "held by user")
}

func (c *Controller) Release(ctx context.Context, id int64) error {
	return c.store.SetHeld(ctx, id, false, "")
}

// Requeue returns a running job to the queue, killing it first.
func (c *Controller) Requeue(ctx context.Context, id int64) error {
	j, err := c.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if j.State == job.Running && j.Node != "" {
		c.enqueue(j.Node, agentapi.Action{Kind: agentapi.ActionKill, JobID: id})
	}
	return c.store.Requeue(ctx, id, "requeued by user")
}

// DrainNode stops a node accepting new work. Running jobs continue.
func (c *Controller) DrainNode(ctx context.Context, name, reason string) error {
	name, err := c.NodeIdentity(ctx, name)
	if err != nil {
		return err
	}
	if reason == "" {
		reason = "drained by admin"
	}
	if err := c.store.SetNodeState(ctx, name, store.NodeDrain, reason); err != nil {
		return err
	}
	c.store.Event(ctx, 0, "node_drain", name+": "+reason, c.now())
	return nil
}

// StopNode asks a node's agent to exit. The node is drained first so the
// scheduler stops sending it work in the interval before it goes.
func (c *Controller) StopNode(ctx context.Context, name string) error {
	name, err := c.NodeIdentity(ctx, name)
	if err != nil {
		return err
	}
	if err := c.store.SetNodeState(ctx, name, store.NodeDrain, "agent stopping"); err != nil {
		return err
	}
	c.enqueue(name, agentapi.Action{Kind: agentapi.ActionStop})
	c.store.Event(ctx, 0, "node_stop", name, c.now())
	return nil
}

// RemoveNode forgets a machine that has left the cluster for good.
//
// The pending-action queue is cleared too: leaving work addressed to a node
// that no longer exists would keep it alive in memory forever, and would be
// delivered to any future machine that joined under the same name.
func (c *Controller) RemoveNode(ctx context.Context, name string) error {
	// Resolved so an admin can forget a machine by the name they gave it.
	// A machine that has already gone may not resolve, in which case the
	// name is used as typed -- forgetting is exactly what you do to
	// something no longer there.
	if id, err := c.NodeIdentity(ctx, name); err == nil {
		name = id
	}
	if err := c.store.DeleteNode(ctx, name); err != nil {
		return err
	}
	// Remembered, or the machine puts itself back. Deleting the row stops
	// nothing: the agent is still running with a valid certificate, and its
	// next heartbeat -- three seconds later -- recreates what was just
	// removed. See HandleHeartbeat, which refuses a forgotten machine and
	// tells it to stop, and the join path, which is how one comes back.
	if err := c.store.ForgetNode(ctx, name, c.now()); err != nil {
		c.log.Warn("could not record that a node was removed; it may come back "+
			"on its next heartbeat", "node", name, "err", err)
	}
	c.mu.Lock()
	delete(c.pending, name)
	c.mu.Unlock()
	c.metrics.Forget(name)
	// A departed machine's files stop counting against anybody's total.
	// Leaving them would keep charging users for space on a machine that is
	// gone, so they could be permanently over a limit with no way back.
	if err := c.store.ForgetNodeStorage(ctx, name); err != nil {
		c.log.Warn("could not forget a departed node's storage index", "node", name, "err", err)
	}
	c.store.Event(ctx, 0, "node_removed", name, c.now())
	return nil
}

func (c *Controller) ResumeNode(ctx context.Context, name string) error {
	name, err := c.NodeIdentity(ctx, name)
	if err != nil {
		return err
	}
	if err := c.store.SetNodeState(ctx, name, store.NodeUp, ""); err != nil {
		return err
	}
	c.store.Event(ctx, 0, "node_resume", name, c.now())
	return nil
}

// ImpossibleRequest returns a human-readable reason when no node in the
// cluster could ever satisfy these limits, or "" when some node could.
//
// Checked against the largest node rather than free capacity: "wait your turn"
// and "this will never fit" are different answers and users deserve the right
// one immediately.
func (c *Controller) ImpossibleRequest(ctx context.Context, l job.Limits,
	nodeList []string, constraint string) string {
	nodes, err := c.store.Nodes(ctx)
	if err != nil || len(nodes) == 0 {
		return "" // nothing registered yet; let it queue
	}
	// A --nodelist naming machines the cluster has never heard of can never be
	// satisfied. Under strict FIFO such a job sits at the head of the queue and
	// blocks everything behind it, so it must be refused rather than queued.
	if len(nodeList) > 0 {
		known := make(map[string]bool, len(nodes))
		all := make([]string, 0, len(nodes))
		for _, n := range nodes {
			known[n.Name] = true
			all = append(all, n.Name)
		}
		var unknown []string
		matched := false
		for _, want := range nodeList {
			if known[want] {
				matched = true
			} else {
				unknown = append(unknown, want)
			}
		}
		if !matched {
			return fmt.Sprintf("--nodelist names no known node (%s); this cluster has: %s",
				strings.Join(unknown, ", "), strings.Join(all, ", "))
		}
	}
	// A constraint no machine can satisfy is the same kind of mistake as a
	// nodelist naming machines that do not exist, and has the same
	// consequence: under strict FIFO the job sits at the head of the queue
	// and everything behind it waits. Told now, with what the cluster
	// actually offers, it is a typo to fix rather than a job that never
	// starts.
	//
	// Judged against every registered machine, including ones that are
	// currently down or drained -- a laptop that is asleep will satisfy
	// "cuda" again when it wakes, and refusing work aimed at it would be
	// wrong.
	if constraint != "" {
		if cc, err := sched.ParseConstraint(constraint); err != nil {
			return err.Error()
		} else if cc != nil {
			match := false
			for _, n := range nodes {
				if cc.Matches(sched.NodeFeatures{
					OS: n.Caps.OS, Arch: n.Caps.Arch, GPUKind: n.Caps.GPUKind,
					Tier: n.Caps.Tier, CPUs: n.Caps.CPUs, MemBytes: n.Caps.MemBytes,
					GPUs: n.Caps.GPUs, GPUMem: n.Caps.GPUMemBytes,
				}) {
					match = true
					break
				}
			}
			if !match {
				return fmt.Sprintf("no machine in this cluster satisfies "+
					"--constraint=%q.\nWhat the machines here offer: %s",
					constraint, clusterFeatures(nodes))
			}
		}
	}
	var maxCPU, maxGPU int
	var maxMem int64
	for _, n := range nodes {
		if n.Caps.CPUs > maxCPU {
			maxCPU = n.Caps.CPUs
		}
		if n.Caps.MemBytes > maxMem {
			maxMem = n.Caps.MemBytes
		}
		if n.Caps.GPUs > maxGPU {
			maxGPU = n.Caps.GPUs
		}
	}
	switch {
	case l.CPUs > maxCPU:
		return fmt.Sprintf("requested %d CPUs; the largest node has %d", l.CPUs, maxCPU)
	case l.MemBytes > maxMem:
		return fmt.Sprintf("requested %d MiB; the largest node has %d MiB", l.MemBytes>>20, maxMem>>20)
	case l.GPUs > maxGPU:
		return fmt.Sprintf("requested %d GPUs; the largest node has %d", l.GPUs, maxGPU)
	}
	return ""
}

// clusterFeatures summarises what the machines here can be selected on.
//
// Printed with a refusal, because "no machine satisfies that" is only half
// an answer: the other half is what they do have, which is otherwise a
// second command away.
func clusterFeatures(nodes []*store.Node) string {
	seen := map[string]bool{}
	var tags []string
	add := func(t string) {
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		tags = append(tags, t)
	}
	for _, n := range nodes {
		add(n.Caps.OS)
		add(n.Caps.Arch)
		add(n.Caps.GPUKind)
		if n.Caps.GPUs > 0 {
			add("gpu")
		}
	}
	sort.Strings(tags)
	return strings.Join(tags, ", ")
}

// NodeInfos renders the registry for sinfo.
func (c *Controller) NodeInfos(ctx context.Context) ([]NodeInfo, error) {
	nodes, err := c.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	states, err := c.NodeStates(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]sched.NodeState{}
	for _, s := range states {
		byName[s.Name] = s
	}
	out := make([]NodeInfo, 0, len(nodes))
	for _, n := range nodes {
		u := byName[n.Name]
		seen := ""
		if !n.LastSeen.IsZero() {
			seen = time.Since(n.LastSeen).Truncate(time.Second).String() + " ago"
		}
		out = append(out, NodeInfo{
			Name: n.Name, State: string(n.State), Reason: n.Reason, LastSeen: seen,
			OS: n.Caps.OS, Arch: n.Caps.Arch, Tier: n.Caps.Tier,
			CPUs: n.Caps.CPUs, UsedCPUs: u.UsedCPUs,
			MemMiB: n.Caps.MemBytes >> 20, UsedMemMiB: u.UsedMem >> 20,
			GPUs:     n.Caps.GPUs,
			MemLimit: string(n.Caps.MemLimit), CPULimit: string(n.Caps.CPULimit),
			Limitations: n.Caps.Lost,
		})
	}
	return out, nil
}

// OutputPath is where a single-node job's output is archived.
func (c *Controller) OutputPath(id int64) string { return c.OutputPathRank(id, 0) }

// OutputPathRank is where one rank's output is archived.
//
// Gang jobs need one file per rank: every node writes its own stdout, and
// collapsing them into a single path means each upload silently overwrites the
// last, leaving one arbitrary rank's output and no indication the rest existed.
func (c *Controller) OutputPathRank(id int64, rank int) string {
	return filepath.Join(c.root, "output", fmt.Sprintf("job-%d.rank%d.out", id, rank))
}

// AuthorizeNodeForJob reports whether a node may upload artefacts for a job,
// and which rank it holds.
//
// A gang job's Node field is a comma-separated list, so comparing it to the
// peer name rejects every rank. Membership must be checked against the task
// table instead.
func (c *Controller) AuthorizeNodeForJob(ctx context.Context, jobID int64, node string) (rank int, ok bool) {
	if r, found := c.store.RankOf(ctx, jobID, node); found {
		return r, true
	}
	j, err := c.store.Get(ctx, jobID)
	if err != nil {
		return 0, false
	}
	return 0, j.Node == node
}

// OutputRanks lists the ranks that have archived output for a job.
func (c *Controller) OutputRanks(id int64) []int {
	var out []int
	for rank := 0; rank < 64; rank++ {
		if _, err := os.Stat(c.OutputPathRank(id, rank)); err == nil {
			out = append(out, rank)
		}
	}
	return out
}

// SaveOutput archives a job's output, uploaded by the node that ran it.
//
// Written to a temp file and renamed, so a failed or partial upload never
// leaves a truncated file that looks like the real output.
func (c *Controller) SaveOutput(id int64, r io.Reader) error {
	return c.SaveOutputRank(id, 0, r)
}

// SaveOutputRank archives one rank's output.
func (c *Controller) SaveOutputRank(id int64, rank int, r io.Reader) error {
	return c.saveTo(c.OutputPathRank(id, rank), r)
}

func (c *Controller) saveTo(dst string, r io.Reader) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// StageInPath / StageOutPath are where a job's transferred archives live on the
// controller.
func (c *Controller) StageInPath(id int64) string {
	return filepath.Join(c.root, "stage", fmt.Sprintf("job-%d.in.tgz", id))
}
func (c *Controller) StageOutPath(id int64) string {
	return filepath.Join(c.root, "stage", fmt.Sprintf("job-%d.out.tgz", id))
}

// SaveStage writes an uploaded archive atomically.
func (c *Controller) SaveStage(path string, r io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "stage-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
