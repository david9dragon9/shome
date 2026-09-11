# Testing the QoS limits

Each script here is designed to break exactly one limit, so you can see the
refusal and confirm it is the one you expected. They are deliberately small and
short; nothing here needs real work to run.

Set up a test account with tight limits, then run `./run.sh`.

```bash
shome admin user add qtest \
  --max-submitted-jobs 4 --max-running-jobs 2 \
  --max-cpus-per-job 2   --max-mem-per-job 512M \
  --max-gpus-per-job 0   --max-walltime 30s \
  --max-procs-per-job 5  --max-disk 10M
```

Then authorise a key for it and run the scripts over ssh as that account, or
run them locally with `SHOME_ACT_AS=qtest` if you are on the controller.

| Script | Limit | What should happen |
|---|---|---|
| `01-too-many-cpus.sh` | `max-cpus-per-job` | refused at submission |
| `02-too-much-memory.sh` | `max-mem-per-job` | refused at submission |
| `03-too-long.sh` | `max-walltime` | refused at submission |
| `04-forbidden-gpu.sh` | `max-gpus-per-job 0` | refused at submission |
| `05-fork-bomb.sh` | `max-procs-per-job` | **runs**, then is killed |
| `06-memory-hog.sh` | job's own `--mem` | **runs**, then killed OUT_OF_MEMORY |
| `07-queue-flood.sh` | `max-submitted-jobs` | first N accepted, rest refused |
| `08-concurrency.sh` | `max-running-jobs` | all accepted, only N run at once |
| `09-disk-fill.sh` | `max-disk` | upload refused partway |

The distinction that matters: limits on *what a job asks for* are refused at
submission, because the answer is knowable then and leaving the job queued
forever would be worse. Limits on *what a job does* can only be caught while it
runs, so those jobs start and are then killed.
