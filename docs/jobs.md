# Running jobs

Three ways to run something, and the only real difference is whether you sit
and watch it.

| | Command | You get |
|---|---|---|
| Batch | `sbatch job.sh` | a job id immediately; walk away |
| Foreground | `srun COMMAND` | the output as it happens, and the exit code |
| Interactive | `srun --pty bash` | a shell on a compute node |

All three take the same resource options and go through the same launch path,
so they get the same sandbox, the same limits and the same enforcement. There
is no second, laxer way in.

## Batch jobs

```console
$ shome sbatch --cpus-per-task 4 --mem 8G --time 2:00:00 train.sh
Submitted batch job 41
```

Come back later:

```bash
shome squeue                 # is it running?
shome cat 41                 # its output
shome sacct                  # how it ended, and what it used
shome scancel 41             # stop it
```

Resource options can live in the script instead, which keeps the submit line
short and the requirements next to the code:

```sh
#!/bin/sh
#SBATCH --job-name=train
#SBATCH --cpus-per-task=4
#SBATCH --mem=8G
#SBATCH --time=2:00:00
python train.py
```

Anything on the command line beats the directive in the file.

**Your script travels with the job.** `sbatch` sends the script's *contents*,
not its path, and the job runs a copy. So the script does not have to exist on
the machine that ends up running it, and it can live anywhere you like. Data is
different, see `docs/storage.md`.

## Foreground jobs

```console
$ shome srun --cpus 4 --mem 8G python -c 'print(2**10)'
srun: job 47 queued
srun: job 47 running on mini
1024

$ shome srun /bin/sh -c 'exit 42'; echo "shell saw $?"
srun: job 48 queued
shome: exited with status 42
shell saw 42
```

It says why it is waiting rather than appearing to hang:

```console
$ shome srun --gres gpu:1 python train.py
srun: job 49 queued
srun: waiting for a GPU (1 in use of 1)
srun: job 49 running on gpu-box
```

stdout and stderr stay separable and the output is archived as usual, so
`shome cat 47` still works afterwards.

## Interactive jobs

`--pty` gives the job a real terminal:

```console
$ shome srun --pty bash
alice@mini.home:~$ pwd
/home/alice
alice@mini.home:~$ python -c 'import torch; print(torch.backends.mps.is_available())'
True
```

```bash
shome srun --pty --gres gpu:1 bash    # an interactive shell with a GPU
shome srun --pty --mem 16G bash
```

## The cluster commands work from inside a job

`squeue`, `squota`, `sbatch` and the rest are on a job's `PATH`, so a task in
an array can see what the rest are doing, and a job about to write a large
result can check its quota first.

One exception: a containerised job reaches the cluster over the network, so a
job submitted without `--network` cannot. It says so rather than pretending the
cluster is down:

```console
$ shome cat 41
shome: the cluster commands cannot be used here:
  a job in a container reaches the cluster over the network, and this one
  was submitted without --network.
  Resubmit with --network, or use 'srun --pty', which has it.
```

## What a job can see

Every job can read the operating system's own program files
and write in its own storage.

A job also gets a scratch directory at `$SHOME_SCRATCH`, which is also
`$TMPDIR`. It is local to that machine, private to that job, and **deleted when
the job ends**, so a machine does not fill up with the remains of finished work.
Anything worth keeping goes in the working directory instead.

## Environment

| Variable | Is |
|---|---|
| `$SHOME_JOB_ID` | this job's id |
| `$SHOME_ARRAY_TASK_ID` | the index, in an array job |
| `$SHOME_SCRATCH` | per-job scratch, deleted at the end. Also `$TMPDIR` |
| `$SHOME_SUBMIT_DIR` | where you were standing when you submitted |
| `$SHOME_HOST` | the machine running this job |
| `$SHOME_CLUSTER` | the cluster's name |
| `$SHOME_VENV` | the right virtualenv for this kind of machine |
| `$SHOME_SLOT` | which kind that is, e.g. `linux-arm64` |

The `SLURM_*` equivalents are set alongside them, so existing scripts work
unchanged.
