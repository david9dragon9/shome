# Storage and moving files

**There is no shared filesystem.** That is the one real difference from a
university cluster, and it is the thing to internalise: your files live on a
particular machine, and the same account has different files on every machine.

So every path names a machine:

```console
$ shome fs ls
MACHINE  SIZE    PATH
mini     4.2 GB  dataset/
mini     102 B   slurm-91.out
gpu-box  1.1 GB  model.bin
```

`mini:dataset` and `gpu-box:dataset` are two different directories that happen
to share a name. Nothing syncs them for you.

## Where a job runs

A job's working directory is your own storage on the machine that runs it.

```console
$ cat job.sh
echo "result" >> results.txt                # kept
echo "scratch" > "$SHOME_SCRATCH/tmp.bin"   # gone when the job ends

$ shome sbatch job.sh
Submitted batch job 91
$ shome fs ls mini:
mini  7 B     results.txt
mini  102 B   slurm-91.out
```

Job output lands there too, as `slurm-<id>.out`, which is where `ls` finds it
and what Slurm calls it. shome keeps its own copy on the controller as well,
which is what `shome cat 91` reads when you are on another machine.

`--chdir` picks a subdirectory and creates it, so jobs need not all write into
the same place:

```bash
shome sbatch --chdir runs/2026-09 job.sh
```

## Moving files

```bash
shome fs ls                        # everything, everywhere
shome fs ls mini:                  # one machine
shome fs du                        # where your space has gone

shome fs cp ./model.bin mini:      # up, from the computer you are on
shome fs cp mini:out.csv .         # down
shome fs cp ./dataset mini:        # a whole directory, like cp -r
shome fs cp mini:runs/2026-09 .    # and back

shome fs cp mini:model.bin gpu-box:   # between machines
shome fs mv mini:model.bin gpu-box:   # copy, then remove the original
shome fs sync ./data mini:data        # only what changed
shome fs sync mini:data gpu-box:data  # make the destination match, one way

shome fs rm mini:old.bin           # delete on one machine
shome fs rm old.bin                # delete everywhere it exists
```

A side with no machine before the colon is a file *or directory* on the
computer you are typing on. That is how work gets in and out.

`cp` on a directory copies the tree a file at a time, so an interrupted copy has
transferred whole files rather than half an archive. `sync` compares both sides
and sends only what is missing or changed, which is what makes putting a
dataset next to a GPU bearable to repeat.

Symlinks are skipped and counted rather than followed: there is no way to record
the link itself, and following it would copy something from outside the tree.

`shome storage` is the same thing scoped to the controller machine, kept because
it is shorter to type when that is where you are. It is the same files and the
same allowance as `shome fs`.

### Getting results back

```bash
shome cat 91                       # a finished job's output
shome fetch 91 .                   # its staged-out results
```

Or stage in and out as part of the job, which is the tidier way when the inputs
are not already on the cluster:

```bash
shome sbatch --stage-in input --stage-out results job.sh
```

## A copy costs its size again

`model.bin` on three machines is three files and three times the space, all
against one limit.

```console
$ shome fs du
MACHINE  USED    FILES  AS OF
mini     4.2 GB  3      2026-09-05 00:08:24
gpu-box  4.0 GB  1      2026-09-05 00:08:23

TOTAL    8.2 GB  4      of 20.0 GB
```

Your storage limit is that cluster-wide total, and it is checked before a
transfer starts rather than discovered halfway through:

```console
$ shome fs cp mini:model.bin gpu-box:
shome: this would put you at 24.0 GiB of your 20.0 GiB cluster-wide storage limit.

Storage is per machine and adds up: a copy costs its size again on
the destination. See where it has gone with 'shome fs du'
```

## Two things worth knowing

**Listings are a cache.** They come from the controller's index, which each
machine rebuilds at most every 20 seconds. A file created *outside* shome may
take that long to appear, and `ls` says when each machine last checked in. 

## Machine owners are unaffected

Cluster files live under shome's own state directory, inside the disk allowance
the machine's owner set, and never in their home directory. An owner can cap
what shome occupies, and a machine over that cap stops being given new work
until space is freed.
