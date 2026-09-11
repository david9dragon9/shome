# shome examples

Each example is a runnable script. They assume `shome` is on
your `PATH` (or run `./examples/run-all.sh`, which builds them and starts a
throwaway cluster for you).

| # | Example | What it shows |
|---|---|---|
| 01 | [hello](01-hello.sh) | Submit a job, watch it, read its output |
| 02 | [limits](02-limits.sh) | `--cpus`, `--mem`, `--time`, and what happens when a job exceeds them |
| 03 | [array](03-array.sh) | A parameter sweep as a job array |
| 04 | [staging](04-staging.sh) | Get data in and results out |
| 05 | [pipeline](05-pipeline.yaml) | A multi-stage DAG |
| 06 | [aggregate](06-aggregate.sh) | Pool several machines for one job |
| 07 | [service](07-service.sh) | A resident service that shome keeps alive |
| 08 | [owner](08-owner.sh) | Reclaim your own machine |
| 09 | [users](09-users.sh) | Multi-tenancy and isolation |
| 10 | [python-sdk](10-python-sdk.py) | Drive shome from Python |
| 11 | [qos](11-qos/) | One script per account limit, each breaking exactly one |

Run everything against a scratch cluster that is deleted afterwards:

```bash
./examples/run-all.sh          # all of 01-10
./examples/run-all.sh 04       # just one
./examples/11-qos/run.sh       # the limits, against a throwaway account
```

`run-all.sh` builds the binary into the throwaway cluster's own directory and
removes both at the end, so it touches no existing installation and leaves no
build output in the repository. It needs Go; everything else needs only
`shome` on your `PATH`.
