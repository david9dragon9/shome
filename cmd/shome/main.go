// Command shome is the user-facing CLI.
//
// The commands and flags mirror Slurm so existing habits and scripts carry
// over. Where shome cannot honour a Slurm guarantee it says so rather than
// pretending -- see `shome sinfo`, which prints enforcement fidelity.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/sched"
	"github.com/davidwu/shome/internal/userenv"
)

func main() {
	cmd, args := dispatch()
	if err := run(cmd, args); err != nil {
		// A request for help is not a failure: print it plainly, exit clean.
		if u, ok := err.(errUsage); ok {
			fmt.Print(string(u))
			return
		}
		// srun exits with the status of the command it ran, so that it
		// composes in a shell script the way any other command does. The
		// message still goes to stderr, but exiting 1 for a command that
		// exited 42 would lose information the caller is entitled to.
		if code, ok := asExitCode(err); ok {
			if code != 0 {
				fmt.Fprintln(os.Stderr, "shome: "+err.Error())
			}
			os.Exit(code)
		}
		fmt.Fprintln(os.Stderr, "shome: "+err.Error())
		os.Exit(1)
	}
}

// dispatch supports both `shome sbatch ...` and a `sbatch` shim symlink.
func dispatch() (string, []string) {
	base := filepath.Base(os.Args[0])
	// Invoked under one of the Slurm names, via a shim symlink. The same
	// list the shims are made from, so adding a command cannot leave the
	// symlink working and the dispatch not.
	for _, n := range userenv.ShimNames {
		if base == n {
			return base, os.Args[1:]
		}
	}
	if len(os.Args) < 2 {
		return "help", nil
	}
	return os.Args[1], os.Args[2:]
}

// localOnlyVerbs manage the shome installation on the machine they run on.
//
// Inside a login session they would act on the session's own bridge
// directory, not on a real installation -- so `nuke` would offer to delete
// part of the caller's home and `token` would look for an admin token that
// is deliberately not there. Refusing with an explanation beats either.
//
// A guardrail, not a security boundary. The boundary is the session's
// credential, which is scoped to one account and which the API checks on
// every request; someone who unsets SHOME_SESSION gains nothing but the
// ability to confuse their own home directory.
var localOnlyVerbs = map[string]string{
	"up": "start a cluster", "start": "start a cluster",
	"down": "stop the daemon", "stop": "stop the daemon",
	"restart": "restart the daemon", "nuke": "remove shome from a machine",
	"join": "join a cluster", "invite": "add a machine",
	"token": "read this machine's admin token", "web": "open the admin console",
	"logs": "read the daemon log", "contribute": "set this machine's limits",
	"pause": "pause this machine", "resume": "resume this machine",
	"shims": "install command shims", "doctor": "diagnose this installation",
}

func run(cmd string, args []string) error {
	if os.Getenv("SHOME_SESSION") == "1" {
		if what, local := localOnlyVerbs[cmd]; local {
			return fmt.Errorf("'shome %s' would %s -- and it manages a machine, "+
				"not a cluster.\n\n"+
				"You are in a cluster session, so there is no local installation "+
				"here to act on.\nRun it on the machine itself.\n\n"+
				"From here: sbatch, srun, squeue, sinfo, fs, storage, plan, whoami.",
				cmd, what)
		}
	}
	switch cmd {
	case "sbatch":
		return sbatch(args)
	case "srun":
		return srun(args)
	case "squeue":
		return squeue(args)
	case "scancel":
		return scancel(args)
	case "sinfo":
		return sinfo(args)
	case "sacct":
		return sacct(args)
	case "scontrol":
		return scontrol(args)
	case "up", "start":
		return up(args)
	case "down", "stop":
		return down(args)
	case "restart":
		return restart(args)
	case "logs":
		return logs(args)
	case "invite":
		return invite(args)
	case "join":
		return join(args)
	case "top", "dash", "dashboard":
		return top(args)
	case "web":
		return webConsole(args)
	case "token":
		return tokenCmd(args)
	case "console":
		return console(args)
	case "nuke":
		return nuke(args)
	case "doctor":
		return doctor(args)
	case "shims":
		return shims(args)
	case "events":
		return events(args)
	case "enroll":
		return enrollCmd(args)
	case "keys":
		return keysCmd(args)
	case "unenroll", "logout":
		return unenrollCmd(args)
	case "squota", "quota", "usage":
		return quota(args)
	case "sshare":
		return sshare(args)
	case "whoami":
		var out map[string]any
		if err := call("GET", "/whoami", nil, &out); err != nil {
			return err
		}
		fmt.Printf("%v (%v)\n", out["user"], out["role"])
		return nil
	case "cat":
		return catOutput(args)
	case "pause":
		return ownerPause(args)
	case "resume":
		return ownerResume(args)
	case "status":
		return ownerStatus(args)
	case "monitor", "mine":
		return monitor(args)
	case "config":
		return ownerConfig(args)
	case "contribute":
		return ownerContribute(args)
	case "serve":
		return serve(args)
	case "pipeline":
		return runPipeline(args)
	case "plan":
		return planCmd(args)
	case "login":
		return sshLogin(args)
	case "fs", "files":
		return fsCmd(args)
	case "storage":
		return storage(args)
	case "fetch":
		return fetchResults(args)
	case "admin":
		return admin(args)
	case "shutdown":
		// Kept because it works remotely, over the API, where 'down' does not.
		if err := call("POST", "/shutdown", nil, nil); err != nil {
			return err
		}
		fmt.Println("controller shutting down")
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `shome - manage your home computers as a cluster

setting up:
  up                        start the cluster on this machine (or rejoin one)
  invite [NAME]             print the one command to add another machine
  join HOST --token T       join a cluster (what 'invite' tells you to run)
  down                      stop shome on this machine
  restart                   stop and start again
  logs [-f] [-n N]          the daemon's log
  nuke [--confirm]          remove shome from this machine, and verify

using the cluster:
  sbatch [opts] script.sh   submit a batch job
  srun [opts] COMMAND       run something now and watch it (--pty for a shell)
  squota                    what you are using, and what you are allowed
  sshare                    fair-share standing, and why jobs are ordered
  squeue [-u USER] [-a]     list jobs (-a includes finished)
  scancel JOBID...          cancel jobs
  sinfo                     node capacity and enforcement fidelity
  sacct [-u USER]           accounting history for finished jobs
  scontrol show job ID      full detail for one job
  shims install DIR         symlink sbatch/squeue/... into DIR
  cat JOBID [--rank N]      print a finished job's output (all ranks by default)
  fetch JOBID [DIR]         download a job's staged-out results
  fs ...                    your files across machines (ls/cp/mv/sync/rm/du)
  storage ...               files on the controller (ls/put/get/rm/du)
  login                     get a short-lived SSH certificate for the login node
  serve ...                 long-running services (create/list/stop/delete)
  pipeline run FILE.yaml    submit a declarative DAG of stages
  top [-i 2s]               live cluster dashboard (interactive)
  web [--open]              the web console's URL and your token
  console [-i 2s]           plain scrolling cluster view
  plan [--total-*]          can this run, and where? (submits nothing)
  whoami                    show which account your token maps to
  token                     print your API token (for the console, or a script)
  enroll                    get a code to add another of your computers
  keys                      which keys can log in as you
  unenroll [--all]          sign this computer out (--all: everywhere)

this machine (works without a cluster connection):
  monitor [-i 2s]           live view of THIS machine in the cluster
  status                    the same, printed once
  doctor                    diagnose this installation
  pause [REASON]            stop contributing now; suspends running jobs
  resume                    contribute again
  config [set NAME VALUE]   what this computer gives the cluster
  contribute [--example]    print an example policy file
  events [-n N]             recent cluster events (audit log)
  admin ...                 cluster administration (see: shome admin)

sbatch options (Slurm-compatible; #SBATCH directives in the script also work):
  -J, --job-name=NAME       -c, --cpus-per-task=N
      --mem=SIZE            -t, --time=LIMIT       (bare --mem number is MB)
      --gres=gpu:N              --network          (network is denied by default)
      --array=0-9[:step][%N]    submit a job array
  -D, --chdir=DIR           run in DIR, relative to your storage (created if
                            missing). Jobs run at the top of it by default.
      --total-cpus=N            aggregate across nodes: total CPUs wanted
      --total-mem=SIZE          aggregate: total memory wanted
      --total-gpu-mem=SIZE      aggregate: total GPU memory wanted
      --total-gpus=N            aggregate: total GPUs wanted
  -N, --nodes=N|auto            cap the node count (default: fewest that fit)
      --requeue                 if the node dies, re-run this job from the
                                START. Only for idempotent work; the default
                                is to fail the job instead.
  -w, --nodelist=NODE[,...]     run only on these machines (by name)
  -C, --constraint=EXPR         require node capabilities, e.g.
                                "metal&gpu_mem>=12G" or "cuda|apple_silicon"
      --model=NAME              model-parallel inference over these weights
      --fabric=NAME             pin the coordination backend
                                (none|ray|mlx|llamacpp-rpc; default: chosen)
      --stage-in=PATH[,...]     upload inputs into the job's working dir
      --stage-out=PATH[,...]    return these paths when the job finishes
  -d, --dependency=afterok:ID   run only after another job succeeds
`)
}

// ---------- transport ----------

// EndpointEnv names a TCP address to reach the controller on, instead of the
// local unix socket.
//
// Set for a session that runs in a container. A unix socket cannot be used
// there: the account's directory reaches the container over virtiofs, which
// exposes the socket's inode but fails the connect with "operation not
// supported" -- so the socket is visible, looks right, and cannot be used.
const EndpointEnv = "SHOME_ENDPOINT"

// apiBase is the URL prefix for a request to the controller.
func apiBase() string {
	if e := strings.TrimSpace(os.Getenv(EndpointEnv)); e != "" {
		return strings.TrimRight(e, "/")
	}
	// The host part is ignored by the unix dialer below; it exists because
	// an HTTP request needs one.
	return "http://shome"
}

func client() *http.Client {
	if strings.TrimSpace(os.Getenv(EndpointEnv)) != "" {
		return &http.Client{}
	}
	sock := ctl.SocketPath(ctl.DefaultRoot())
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

// errEmptyToken is returned when SHOME_TOKEN is set to nothing.
//
// Set-but-empty is a mistake worth stopping on rather than working around:
// the usual cause is a shell variable that did not get filled in --
// `SHOME_TOKEN=$(mint_a_token) shome scancel 41` where the minting failed --
// and the fallbacks below would then run that command as the machine's
// owner, who is a cluster administrator. A script meant to act as one
// account would quietly act as everybody. Nothing about the output would
// say so, and on the controller host it would succeed.
var errEmptyToken = fmt.Errorf("SHOME_TOKEN is set but empty.\n" +
	"  Something meant to supply a token produced nothing. Unset it to use\n" +
	"  your own credential; leaving it empty would run this as the cluster owner.")

// authToken finds the caller's API token.
//
// Order matters: an explicit SHOME_TOKEN wins, then a per-user token file, then
// the cluster owner's bootstrap token. The last one means the owner's CLI works
// out of the box on the controller machine, while a remote user must be given
// a token explicitly.
func authToken() (string, error) {
	if v, ok := os.LookupEnv("SHOME_TOKEN"); ok {
		if t := strings.TrimSpace(v); t != "" {
			return t, nil
		}
		return "", errEmptyToken
	}
	if home, err := os.UserHomeDir(); err == nil {
		if b, err := os.ReadFile(filepath.Join(home, ".shome", "token")); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	if b, err := os.ReadFile(ctl.AdminTokenPath(ctl.DefaultRoot())); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	return "", nil
}

// validToken returns the caller's token, rejecting anything that cannot go in
// an HTTP header.
//
// Without this, a token pasted with a stray newline produces
// "invalid header field value", which net/http reports as a transport error --
// so the CLI blames an unreachable daemon for what is really a bad credential.
// setActAs forwards the login shell's authenticated principal. Only honoured
// by the controller for admin tokens, and logged every time.
func setActAs(req *http.Request) {
	if as := strings.TrimSpace(os.Getenv("SHOME_ACT_AS")); as != "" {
		req.Header.Set("X-Shome-Act-As", as)
	}
}

func validToken() (string, error) {
	t, err := authToken()
	if err != nil {
		return "", err
	}
	if t == "" {
		return "", fmt.Errorf("no API token found.\n" +
			"  This machine can reach the cluster, but nothing here says who you are.\n" +
			"  On the controller, or in a login session, run 'shome token' and then\n" +
			"  either export SHOME_TOKEN=... or write it to ~/.shome/token here.\n" +
			"  On the controller host itself, its owner needs neither.")
	}
	for _, r := range t {
		if r < 0x20 || r > 0x7e {
			return "", fmt.Errorf("API token contains an invalid character; " +
				"check for a stray newline or that you copied only the token")
		}
	}
	return t, nil
}

// apiUnavailable is the node's explanation for why this job cannot reach
// the cluster, when it knows one.
//
// Set by the agent for a contained job submitted without --network: the
// commands are on its PATH and there is no route for them to use. Checked
// before dialling, because what follows otherwise is a connection error
// naming the controller, and the controller is not the problem.
func apiUnavailable() error {
	why := strings.TrimSpace(os.Getenv("SHOME_API_UNAVAILABLE"))
	if why == "" {
		return nil
	}
	return fmt.Errorf("the cluster commands cannot be used here:\n  %s", why)
}

func call(method, path string, body, out any) error {
	if err := apiUnavailable(); err != nil {
		return err
	}
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, apiBase()+path, rdr)
	if err != nil {
		return err
	}
	tok, err := validToken()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	setActAs(req)
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach shomectld (is it running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// callRaw fetches a response body without JSON decoding, for job output.
func callRaw(method, path string) ([]byte, error) {
	if err := apiUnavailable(); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, apiBase()+path, nil)
	if err != nil {
		return nil, err
	}
	tok, err := validToken()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	setActAs(req)
	resp, err := client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach shomectld (is it running?): %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return body, nil
}

// postRaw uploads a body without JSON encoding, for archive transfers.
func postRaw(path string, body io.Reader) error {
	req, err := http.NewRequest("POST", apiBase()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	tok, err := validToken()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	setActAs(req)
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

// callStream is call() with a raw request body, for file uploads. A file may
// be far larger than memory, so the body is streamed rather than marshalled.
func callStream(method, path string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, apiBase()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	tok, err := validToken()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	setActAs(req)
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach shomectld (is it running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s", resp.Status)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// postStream sends a stream of bytes as a request body and does not wait for
// the reader to end before the request starts.
//
// Used for keystrokes: the body is a terminal, so it never "ends" until the
// session does, and buffering it would mean nothing reached the far side.
func postStream(path string, body io.Reader) error {
	req, err := http.NewRequest("POST", apiBase()+path, io.NopCloser(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	tok, err := validToken()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	setActAs(req)
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return nil
}

// openStream starts a download the caller copies out itself, so a large file
// never has to fit in memory. The caller closes it.
func openStream(path string) (io.ReadCloser, error) {
	req, err := http.NewRequest("GET", apiBase()+path, nil)
	if err != nil {
		return nil, err
	}
	tok, err := validToken()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	setActAs(req)
	resp, err := client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach shomectld (is it running?): %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("%s", e.Error)
		}
		return nil, fmt.Errorf("%s", resp.Status)
	}
	return resp.Body, nil
}

// maxScriptBytes caps a submitted script. It is a script, not a data payload;
// anything larger belongs in --stage-in.
const maxScriptBytes = 1 << 20

// dedupe removes repeats while preserving order.
func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

// ---------- sbatch ----------

// parseOpts handles both "--flag=value" and "--flag value", plus short forms.
// Written by hand rather than with the flag package because directives from the
// script and arguments from the command line must go through identical parsing.
func parseOpts(argv []string, spec *job.Spec, array *string, stageIn *[]string) ([]string, error) {
	var rest []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		val := ""
		hasVal := false
		if j := strings.Index(name, "="); j >= 0 {
			name, val, hasVal = name[:j], name[j+1:], true
		}
		switch name {
		case "J", "job-name":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--job-name needs a value")
				}
				val = argv[i]
			}
			spec.Name = val
			continue
		case "network":
			spec.Limits.Network = true
			continue
		case "requeue":
			// Opt in to being restarted from scratch after a node is lost.
			spec.Requeue = true
			continue
		case "no-requeue":
			spec.Requeue = false
			continue
		case "w", "nodelist":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--nodelist needs a value")
				}
				val = argv[i]
			}
			for _, n := range strings.Split(val, ",") {
				if n = strings.TrimSpace(n); n != "" {
					spec.NodeList = append(spec.NodeList, n)
				}
			}
			continue
		case "C", "constraint":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--constraint needs a value")
				}
				val = argv[i]
			}
			if _, err := sched.ParseConstraint(val); err != nil {
				return nil, err
			}
			spec.Constraint = val
			continue
		case "fabric":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--fabric needs a value")
				}
				val = argv[i]
			}
			spec.Fabric = val
			continue
		case "model":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--model needs a value")
				}
				val = argv[i]
			}
			spec.Model = val
			continue
		case "total-cpus":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--total-cpus needs a value")
				}
				val = argv[i]
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("--total-cpus needs a positive integer")
			}
			spec.TotalCPUs = n
			continue
		case "total-mem":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--total-mem needs a value")
				}
				val = argv[i]
			}
			b, err := ctl.ParseMem(val)
			if err != nil {
				return nil, err
			}
			spec.TotalMemBytes = b
			continue
		case "total-gpu-mem":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--total-gpu-mem needs a value")
				}
				val = argv[i]
			}
			b, err := ctl.ParseMem(val)
			if err != nil {
				return nil, err
			}
			spec.TotalGPUMem = b
			continue
		case "total-gpus":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--total-gpus needs a value")
				}
				val = argv[i]
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("--total-gpus needs a positive integer")
			}
			spec.TotalGPUs = n
			continue
		case "N", "nodes":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--nodes needs a value")
				}
				val = argv[i]
			}
			if val == "auto" {
				spec.MaxNodes = 0 // let the solver choose
				continue
			}
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("--nodes needs a positive integer or 'auto'")
			}
			spec.MaxNodes = n
			continue
		case "stage-in":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--stage-in needs a path")
				}
				val = argv[i]
			}
			*stageIn = append(*stageIn, strings.Split(val, ",")...)
			continue
		case "stage-out":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--stage-out needs a path")
				}
				val = argv[i]
			}
			spec.StageOut = append(spec.StageOut, strings.Split(val, ",")...)
			continue
		case "d", "dependency":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--dependency needs a value")
				}
				val = argv[i]
			}
			spec.Dependency = val
			continue
		case "D", "chdir":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--chdir needs a path")
				}
				val = argv[i]
			}
			// Checked here so a mistake is a message now, rather than a
			// job that queues and then fails. The agent checks it again;
			// see job.CleanChdir.
			clean, err := job.CleanChdir(val)
			if err != nil {
				return nil, err
			}
			spec.Chdir = clean
			continue
		case "a", "array":
			if !hasVal {
				i++
				if i >= len(argv) {
					return nil, fmt.Errorf("--array needs a value")
				}
				val = argv[i]
			}
			*array = val
			continue
		}
		if !hasVal {
			i++
			if i >= len(argv) {
				return nil, fmt.Errorf("--%s needs a value", name)
			}
			val = argv[i]
		}
		if err := ctl.ApplyLimit(&spec.Limits, name, val); err != nil {
			return nil, err
		}
	}
	return rest, nil
}

func sbatch(args []string) error {
	spec := job.Spec{User: currentUser(), Limits: job.Limits{CPUs: 1}}

	// Command line is parsed first only to find the script; directives are
	// applied next, then the command line again so it wins on conflict --
	// matching Slurm's precedence.
	var array string
	var stageIn []string
	rest, err := parseOpts(args, &spec, &array, &stageIn)
	if err != nil {
		return err
	}
	// A script on stdin, when no path is given or the path is "-".
	//
	// This is what makes the login node usable: someone on another machine has
	// their script locally and no filesystem in common with the cluster, so
	// `cat job.sh | ssh cluster sbatch` is the natural way to submit. Reading
	// a path would require them to upload it first.
	var body []byte
	script := ""
	fromStdin := len(rest) == 0 || rest[0] == "-"
	if fromStdin {
		if len(rest) == 0 && termStdin() {
			return fmt.Errorf("no batch script given\n\n" +
				"  shome sbatch job.sh          submit a script by path\n" +
				"  shome sbatch < job.sh        or read it from standard input\n" +
				"  cat job.sh | ssh cluster sbatch")
		}
		var err error
		body, err = io.ReadAll(io.LimitReader(os.Stdin, maxScriptBytes+1))
		if err != nil {
			return fmt.Errorf("read script from stdin: %w", err)
		}
		if len(body) == 0 {
			return fmt.Errorf("the script on stdin is empty")
		}
		// A name is needed for display; the script has no filename to use.
		script = "stdin"
		if spec.Name == "" {
			spec.Name = "stdin"
		}
	} else {
		var err error
		script, err = filepath.Abs(rest[0])
		if err != nil {
			return err
		}
		if _, err := os.Stat(script); err != nil {
			return fmt.Errorf("script %s: %w", rest[0], err)
		}
	}

	dirs, err := scriptDirectives(script, body, fromStdin)
	if err != nil {
		return err
	}
	if _, err := parseOpts(dirs, &spec, &array, &stageIn); err != nil {
		return fmt.Errorf("in #SBATCH directives: %w", err)
	}
	if _, err := parseOpts(args, &spec, &array, &stageIn); err != nil {
		return err
	}

	// parseOpts runs three times (command line, directives, command line
	// again) so the command line wins on conflict. Slice options append, so
	// they accumulate across passes and must be deduplicated.
	spec.NodeList = dedupe(spec.NodeList)
	spec.StageOut = dedupe(spec.StageOut)
	stageIn = dedupe(stageIn)

	spec.Script = script
	// Send the script's contents, not just its path: the job runs a copy in
	// its own scratch, so a script in your home directory (which the sandbox
	// denies) or on a machine the node cannot see still works.
	if !fromStdin {
		body, err = os.ReadFile(script)
		if err != nil {
			return fmt.Errorf("read %s: %w", script, err)
		}
	}
	if len(body) > maxScriptBytes {
		return fmt.Errorf("script is %d bytes; the limit is %d. "+
			"Put large payloads in --stage-in rather than the script",
			len(body), maxScriptBytes)
	}
	spec.ScriptBody = body
	if spec.Name == "" {
		spec.Name = filepath.Base(script)
	}
	if spec.Workdir == "" {
		spec.Workdir, _ = os.Getwd()
	}

	spec.StageIn = len(stageIn) > 0
	if spec.StageIn && array != "" {
		return fmt.Errorf("--stage-in with --array is not supported yet: " +
			"every task would need its own copy of the inputs")
	}
	// Checked before submitting, so a mistyped input is a message rather
	// than a held job nobody comes back for.
	if spec.StageIn {
		if err := checkStageIn(stageIn); err != nil {
			return err
		}
	}

	var v ctl.JobView
	// With inputs to upload, submit held so the job cannot start before they
	// are in place, then release once the upload lands.
	if err := call("POST", "/submit",
		ctl.SubmitRequest{Spec: spec, Array: array, Hold: spec.StageIn}, &v); err != nil {
		return err
	}
	if spec.StageIn {
		if err := uploadStageIn(v.ID, stageIn); err != nil {
			// The job cannot run without its inputs and the caller has been
			// told, so it is cancelled rather than left held for somebody to
			// find. Only if that also fails does it stay, and then the
			// message says so.
			if cerr := call("POST", fmt.Sprintf("/cancel/%d", v.ID), nil, nil); cerr != nil {
				return fmt.Errorf("stage-in upload failed, and job %d could not be "+
					"cancelled (it is held; cancel it with 'shome scancel %d'): %w",
					v.ID, v.ID, err)
			}
			return fmt.Errorf("stage-in upload failed, so job %d was cancelled: %w", v.ID, err)
		}
		if err := call("POST", fmt.Sprintf("/job/%d/release", v.ID), nil, nil); err != nil {
			return fmt.Errorf("inputs uploaded but release failed: %w", err)
		}
	}
	if v.ArrayTasks > 0 {
		fmt.Printf("Submitted batch job %d (array with %d tasks)\n", v.ID, v.ArrayTasks)
	} else {
		fmt.Printf("Submitted batch job %d\n", v.ID)
	}
	if v.Note != "" {
		fmt.Fprintf(os.Stderr, "\nnote: %s\n", wrapText(v.Note, 76))
	}
	return nil
}

// ---------- squeue ----------

func squeue(args []string) error {
	user := ""
	all := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-u", "--user":
			i++
			if i < len(args) {
				user = args[i]
			}
		case "-a", "--all":
			all = true
		case "--me":
			user = currentUser()
		}
	}
	q := "/jobs"
	if !all {
		q += "?active=1"
	}
	if user != "" {
		if strings.Contains(q, "?") {
			q += "&user=" + user
		} else {
			q += "?user=" + user
		}
	}
	var jobs []ctl.JobView
	if err := call("GET", q, nil, &jobs); err != nil {
		return err
	}
	// The PRIORITY column appears only when something is scored. With
	// priority off every value would be an identical blank, and a column of
	// blanks implies the cluster is using a number it is not.
	scored := false
	for _, j := range jobs {
		if j.Scored {
			scored = true
			break
		}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	head := "JOBID\tNAME\tUSER\tSTATE\tTIME\tTIMELIMIT\tCPUS\tMEM(MiB)\tGPUS\tNODE"
	if scored {
		head += "\tPRIORITY"
	}
	fmt.Fprintln(w, head+"\tREASON")
	for _, j := range jobs {
		reason := j.Reason
		// A pending job with no reason means the scheduler failed to explain
		// itself; surface that rather than showing a blank cell.
		if reason == "" && j.State == "PENDING" {
			reason = "(none recorded)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s",
			j.Label, j.Name, j.User, j.State, j.Elapsed, j.TimeLim,
			j.CPUs, j.MemMiB, j.GPUs, j.Node)
		if scored {
			p := "-"
			if j.Scored {
				p = fmt.Sprintf("%.4f", j.Priority)
			}
			fmt.Fprintf(w, "\t%s", p)
		}
		fmt.Fprintf(w, "\t%s\n", reason)
	}
	if len(jobs) == 0 {
		fmt.Fprintln(w, "(no jobs)")
	}
	return w.Flush()
}

// ---------- scancel ----------

func scancel(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("scancel needs at least one job id")
	}
	var firstErr error
	for _, a := range args {
		id, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scancel: invalid job id %q\n", a)
			if firstErr == nil {
				firstErr = fmt.Errorf("invalid job id %q", a)
			}
			continue
		}
		if err := call("POST", "/cancel/"+strconv.FormatInt(id, 10), nil, nil); err != nil {
			fmt.Fprintf(os.Stderr, "scancel: job %d: %v\n", id, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Printf("cancelled job %d\n", id)
	}
	return firstErr
}

// ---------- sinfo ----------

func sinfo(args []string) error {
	var nodes []ctl.NodeInfo
	if err := call("GET", "/node", nil, &nodes); err != nil {
		return err
	}
	if len(nodes) == 0 {
		fmt.Println("no nodes registered. add one with: shome admin token")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tSTATE\tOS\tARCH\tTIER\tCPUS(used/tot)\tMEM MiB(used/tot)\tGPUS\tSEEN\tREASON")
	for _, n := range nodes {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%d/%d\t%d\t%s\t%s\n",
			n.Name, n.State, n.OS, n.Arch, n.Tier, n.UsedCPUs, n.CPUs,
			n.UsedMemMiB, n.MemMiB, n.GPUs, n.LastSeen, n.Reason)
	}
	w.Flush()

	// Enforcement fidelity is printed unconditionally: users are entitled to
	// know which limits are real guarantees and which are best-effort.
	for _, n := range nodes {
		if len(n.Limitations) == 0 && n.MemLimit == "" {
			continue
		}
		fmt.Printf("\n%s enforcement: memory=%s cpu=%s\n", n.Name, n.MemLimit, n.CPULimit)
		for _, l := range n.Limitations {
			fmt.Printf("  - %s\n", l)
		}
	}
	return nil
}

// ---------- events ----------

func events(args []string) error {
	n := 20
	for i := 0; i < len(args); i++ {
		if args[i] == "-n" && i+1 < len(args) {
			if v, err := strconv.Atoi(args[i+1]); err == nil {
				n = v
			}
		}
	}
	var evs []struct {
		Seq    int64  `json:"Seq"`
		At     string `json:"At"`
		JobID  int64  `json:"JobID"`
		Kind   string `json:"Kind"`
		Detail string `json:"Detail"`
	}
	if err := call("GET", "/events?n="+strconv.Itoa(n), nil, &evs); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tJOB\tEVENT\tDETAIL")
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		ts := e.At
		if len(ts) > 19 {
			ts = ts[:19]
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", ts, e.JobID, e.Kind, e.Detail)
	}
	return w.Flush()
}

// scriptDirectives reads #SBATCH lines from a script, whether it arrived as a
// path or on standard input.
func scriptDirectives(path string, body []byte, fromStdin bool) ([]string, error) {
	if fromStdin {
		return ctl.ParseDirectives(body), nil
	}
	return ctl.ScriptDirectives(path)
}

// termStdin reports whether stdin is a terminal, meaning nothing was piped in.
//
// Without this, `shome sbatch` typed at a prompt would sit waiting for input
// that will never come, looking like a hang rather than a usage error.
func termStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
