# How scheduling works

You do not pick a machine. You say what the job needs, and shome puts it on a
machine that has it.

```bash
shome sbatch --cpus-per-task 4 --mem 8G --time 2:00:00 job.sh
```

A job is placed when some machine has all of what it asked for free at once. If
nothing does, it waits, and `squeue` says what it is waiting for.

Names are a poor way to target a mixed cluster, so ask for the capability
instead:

```bash
shome sbatch --constraint "metal&gpu_mem>=12G" job.sh
shome sbatch --constraint "cuda|apple_silicon" job.sh
```

`--nodelist mini` pins a job to one machine. That is usually a mistake, and
worth it only when the data is already there.

## What you ask for

| Option | Means |
|---|---|
| `-c`, `--cpus-per-task=N` | cores. Default 1 |
| `--mem=SIZE` | memory. A bare number is MB, as in Slurm |
| `--gres=gpu:N` | accelerators |
| `-t`, `--time=LIMIT` | walltime, after which the job is killed |
| `--network` | outbound network. **Denied by default** |
| `-C`, `--constraint=EXPR` | required capabilities |
| `-w`, `--nodelist=NODE` | this machine only |

`#SBATCH` directives in the script work too, and lose to anything you pass on
the command line.

## Give jobs a time limit

`--time` is optional and worth setting anyway. It is the only way shome can
tell that a job will be out of the way soon, so an untimed job can only use
capacity nobody is waiting for. An untimed job also inherits your account's
walltime ceiling, so it is never truly unbounded.

## Queue order

With one person, the queue is submission order, and that is the right answer.

With several, an admin can turn on fair-share ordering, which puts the account
that has used the cluster least recently first. Heavy use lowers your priority,
idleness raises it, and old use counts for less. Nothing is withheld: it only
decides who goes first when two people both have work queued. `sshare` shows
where you stand and `squeue` shows a `PRIORITY` column once it is on.

A big job at the head of the queue would otherwise stall every small job behind
it on idle machines, so a shorter job may use the gap if it can be shown to
finish before the waiting job needs the machine. That is the other reason to
set `--time`.

## When a machine goes away

Losing sight of a machine is not the same as losing the job.

| When | What happens |
|---|---|
| A machine misses heartbeats for 30s | It stops getting **new** work. Jobs already on it keep running, marked "node unreachable" |
| It comes back, job still running | Nothing was disturbed. The note clears. This is the shut-laptop case |
| It comes back, job finished meanwhile | Its real result is recorded. The machine is authoritative about its own jobs |
| It stays gone for 10 minutes | The job is written off as `FAILED` |

A written-off job is failed rather than re-run, because restarting work that
already had effects is worse than failing it: a job that wrote 300 of 1000
records and restarts writes 1300. Pass `--requeue` when a job really is safe to
repeat from the beginning.

## The owner comes first

Every machine belongs to somebody, and they can cap what the cluster may take,
restrict it to certain hours, or stop it outright with `shome pause`. A job on a
machine whose owner pauses it is **suspended, not killed**, and continues when
they resume. So a machine may have idle cores that your job is not offered, and
that is the arrangement working rather than failing.
