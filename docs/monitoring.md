# Watching what is going on

Five things, roughly in the order you will want them.

| | |
|---|---|
| `squeue` | what is queued and running now |
| `sacct` | what finished, and what it used |
| `sinfo` | the machines, and how much of each is free |
| `shome top` | a live dashboard in the terminal |
| `shome web` | the same in a browser |

## squeue: what is happening now

```console
$ squeue
JOBID  NAME      USER   STATE    TIME      TIMELIMIT  CPUS  MEM(MiB)  GPUS  NODE
41     train.sh  alice  RUNNING  00:01:12  2:00:00    1     8192      1     studio
42     sweep     alice  PENDING  00:00:04  30:00      4     4096      0     -
```

```bash
squeue                  # everything active
squeue --me             # just yours
squeue -u bob           # somebody else's
squeue -a               # include finished jobs
```


```console
$ squeue
JOBID  NAME   STATE    REASON
54     train  PENDING  at your limit of 2 running job(s) (max-running-jobs)
55     big    PENDING  waiting for CPUs to free
```

For one job in full, including why it has the priority it has:

```bash
shome scontrol show job 54
```

## sacct: what happened

`squeue` drops a job once it ends. `sacct` is the history, with the columns you
want after the fact:

```console
$ sacct
JOBID  NAME      USER   STATE      EXIT  ELAPSED   REQ MEM   PEAK MEM  REASON
41     train.sh  alice  COMPLETED  0     00:41:09  8192 MiB  5140 MiB
39     hog       alice  OUT_OF_MEMORY 0  00:00:12  256 MiB   642 MiB   exceeded memory limit
38     sweep     alice  FAILED     1     00:00:03  1024 MiB  12 MiB
```

```bash
sacct                   # finished jobs
sacct --me
sacct -u bob
```

Read a finished job's output with:

```bash
shome cat 41
```

## sinfo: the machines

```console
$ sinfo
NODE     STATE  OS      ARCH   TIER   CPUS(used/tot)  MEM MiB(used/tot)  GPUS  SEEN     REASON
studio   UP     darwin  arm64  full   4/10            8192/32768         1     2s ago
mini     UP     darwin  arm64  full   0/8             0/16384            1     3s ago
gpu-box  DRAIN  linux   amd64  full   0/16            0/65536            1     4s ago   owner: away until Monday
```

- **STATE**: `UP` takes work, `DRAIN` finishes what it has but accepts nothing
  new, `DOWN` has missed heartbeats.
- **used/tot**: what is allocated against what the machine's owner lends the
  cluster, which is not necessarily what the machine physically has.
- **TIER**: how hard that machine's limits are. `full` means kernel-enforced;
  anything less means the guarantees are weaker, and the machine says which
  ones. See `docs/scheduling.md`.
- **SEEN**: how long ago it last checked in. A growing figure is a machine
  going away.
- **REASON**: why it is drained or down. Usually its owner wanting their
  computer back, which is the arrangement working.

## shome top: the live dashboard

```bash
shome top
shome top -i 1s      # refresh faster
shome top -1         # one frame and exit, for a script or a pipe
```

Three panes: cluster totals, the machines, and the queue. Cores and memory each
show two gauges: **used**, what is actually being consumed, and **alloc**, what
is reserved. A machine can be fully allocated and nearly idle, which is a job
that asked for more than it needs.

It is interactive, and acts on the cluster rather than only reporting on it:

| Key | Does |
|---|---|
| `tab` | switch between the queue and the machines |
| `↑` `↓` | select a row |
| `k` | kill the selected job |
| `h` | hold a pending job, or release a held one |
| `r` | requeue a job |
| `d` / `u` | drain or resume the selected machine |
| `/` | filter |
| `p` | pause refreshing, to read something without it moving |
| `+` `-` | refresh faster or slower |
| `?` | help |
| `q` | quit |

`shome console` is the same data as plain scrolling output, for a terminal that
cannot do full-screen or a log you want to keep.

## shome web: the browser version

```console
$ shome web
Web console  http://127.0.0.1:7820/

Token        alice.xR4k...

Open the URL and paste the token. It is your API credential:
treat it like a password, and do not paste it anywhere else.

The console listens on loopback only. To reach it from another
machine, tunnel rather than exposing it:
  ssh -N -L 7820:127.0.0.1:7820 you@studio
```

`shome web --open` launches a browser as well. It runs on the controller, so run
it there.

Same data as `shome top`, same actions, in tabs: overview, nodes, jobs,
priority, access and audit.

## Your own machine

The commands above are about the cluster. If you have lent a machine and want to
know what the cluster is doing to *it*:

```bash
shome monitor        # live, interactive
shome status         # the same, printed once
```

## When something is wrong

```bash
shome doctor         # diagnose this installation
shome logs -f        # the daemon's log here
shome events         # recent cluster events
```