# Limits

Every account has limits on what it may ask for and what it may use at once.
One command shows all of them and where you stand:

```bash
squota
```

```console
$ squota
usage for alice

RIGHT NOW                USED    LIMIT
jobs queued and running  1       200       [....................]   0%
jobs running             1       10        [##..................]  10%
cpu cores in use         3       64        [....................]   5%
gpus in use              0       4         [....................]   0%
memory in use            2.0 GB  256.0 GB  [....................]   1%

STORAGE              USED  LIMIT
  mini               4 KB
  gpu-box            2 GB
total, all machines  2 GB  100.0 GB  [....................]   2%

PER JOB            LIMIT
cpu cores per job  32
gpus per job       4
memory per job     128.0 GB
processes per job  4096
walltime per job   7-00:00:00

fair-share factor 1.00 of 1.00
You have not used the cluster recently, so your jobs go ahead of those who have.
```

It is also `shome quota`, and it is on your `PATH` inside a job and a login
session. An admin can ask about somebody else with `squota NAME`; you always
get your own figures whatever you ask for.

## What the limits are

| Limit | Bounds |
|---|---|
| `max-cpus-per-job` | cores one job may ask for |
| `max-mem-per-job` | memory one job may ask for |
| `max-gpus-per-job` | accelerators one job may ask for |
| `max-walltime` | how long one job may run |
| `max-submitted-jobs` | jobs queued and running at once |
| `max-running-jobs` | jobs actually running at once |
| `max-running-cpus` | cores in use across all your jobs |
| `max-running-mem` | memory in use across all your jobs |
| `max-running-gpus` | accelerators in use across all your jobs |
| `max-procs-per-job` | processes one job may have |
| `max-disk` | your files, **totalled across every machine** |

There is a second layer above yours: a cluster-wide total across everybody.
Per-account limits multiply so the cluster total is
what actually bounds it. Your limits can never exceed it, and a refusal says
which layer refused you, so you know whether to wait or to ask an admin.

## Where each one bites

Two moments, because the limits divide in two.

**What a job asks for is refused at submission**, with the number it exceeded:

```console
$ shome sbatch --cpus-per-task 64 job.sh
shome: --cpus-per-task 64 exceeds your limit of 32 (max-cpus-per-job)
```

That is `max-cpus-per-job`, `max-mem-per-job`, `max-gpus-per-job`,
`max-walltime` and `max-submitted-jobs`.

**What you are using is checked when a job is about to start.** A job over the
line stays queued with the reason, and starts on its own as your other work
finishes:

```console
$ squeue
JOBID  NAME     STATE    REASON
54     train    PENDING  at your limit of 2 running job(s) (max-running-jobs)
```

That is `max-running-jobs`, `max-running-cpus`, `max-running-mem`,
`max-running-gpus`, `max-procs-per-job` and `max-disk`.


## The two that can only be caught while a job runs

**Memory.** A job over its `--mem` is killed and ends `OUT_OF_MEMORY`, with the
peak it reached:

```console
$ squeue -a
JOBID  NAME     STATE          REASON
15     hog      OUT_OF_MEMORY  exceeded memory limit (642 MiB used, 256 MiB requested)
```

**Processes.** `max-procs-per-job` is decided by the controller and travels with
the job, because only the machine running it can count. The job starts and is
killed if its process tree grows past the limit:

```console
14     forks    FAILED         exceeded the process limit (25 processes, limit 5)
```

A fork bomb is what it exists for: the one failure that takes a machine down
hard enough that its owner notices, which on a cluster of other people's
computers is what ends the arrangement.

## Storage works differently

Nothing can stop a job writing into its own directory: there are no OS-level
accounts and therefore no filesystem quota. So being over your storage limit
does not fail the current write, it **stops the next job starting**:

```console
$ squeue
JOBID  NAME     STATE    REASON
53     train    PENDING  your files are over your storage limit (184 MiB used,
                         5 MiB allowed); free some with 'shome fs rm' (max-disk)
```

Held rather than failed, so freeing space makes it runnable with nothing to
resubmit. Uploads and transfers are refused outright before they start, since
there the answer is knowable in advance.

Remember that storage is per machine and adds up: a copy costs its size again
on the destination, against the same cluster-wide total. `shome fs du` shows
where it has gone.


## Asking for more

```bash
shome admin qos show alice      # what applies, and what is in use
```

An admin raises a limit with `shome admin qos set`. Worth checking `sacct`
first: it shows the peak memory your finished jobs actually used, and the
answer is often that the request was too big rather than the limit too small.
