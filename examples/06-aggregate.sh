#!/bin/sh
# Pooling several machines for one job.
#
# The inversion that makes this usable: you ask for TOTALS and shome solves
# placement, instead of you working out which four machines add up to what you
# need and passing -N4 with a hand-computed per-node figure.
set -e

echo "=== what could run, and where? (submits nothing) ==="
shome plan --total-cpus 8 || true
echo
echo "=== an impossible request says so, rather than queuing forever ==="
shome plan --total-gpu-mem 500G 2>&1 | sed 's/^/  /' || true
echo
echo "=== target capability, not hostname ==="
echo "-- a Mac GPU with >= 4 GiB --"
shome plan --total-cpus 2 --constraint "apple_silicon&gpu_mem>=4G" 2>&1 | head -2 | sed 's/^/  /' || true
echo "-- something with CUDA (no such node here) --"
shome plan --total-cpus 2 --constraint "cuda" 2>&1 | head -2 | sed 's/^/  /' || true

echo
echo "=== run one job across however many machines it takes ==="
cat > /tmp/shome-gang.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=gang
# Every rank gets its wiring, under both shome and SLURM names, plus
# MASTER_ADDR/MASTER_PORT for torchrun and friends.
echo "rank $SHOME_NODEID of $SHOME_NNODES on $(hostname)"
echo "  nodes:  $SHOME_NODELIST"
echo "  master: $SHOME_MASTER_ADDR:$SHOME_MASTER_PORT"
JOB
chmod +x /tmp/shome-gang.sh

# --nodes=auto is the default; shome uses the fewest machines that fit, because
# every extra node is a network hop on every collective.
shome sbatch --total-cpus 8 /tmp/shome-gang.sh

for _ in $(seq 1 40); do
  state=$(shome squeue -a | awk '$2=="gang" {print $4; exit}')
  case "$state" in COMPLETED|FAILED|CANCELLED) break;; esac
  sleep 1
done
id=$(shome squeue -a | awk '$2=="gang" {print $1; exit}')
echo
echo "--- output from every rank ---"
shome cat "$id"
