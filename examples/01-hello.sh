#!/bin/sh
# The smallest useful thing: submit a job, wait, read its output.
set -e

cat > /tmp/shome-hello.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=hello
echo "running on $(hostname)"
echo "shome job id: $SHOME_JOB_ID (SLURM_JOB_ID=$SLURM_JOB_ID works too)"
echo "working dir:  $(pwd)"
JOB
chmod +x /tmp/shome-hello.sh

echo "--- submit ---"
shome sbatch /tmp/shome-hello.sh

sleep 2
echo "--- queue ---"
shome squeue

# Jobs finish asynchronously; poll until this one is done.
for _ in $(seq 1 30); do
  state=$(shome squeue -a | awk '$2=="hello" {print $4; exit}')
  case "$state" in COMPLETED|FAILED|CANCELLED) break;; esac
  sleep 1
done

echo "--- output ---"
id=$(shome squeue -a | awk '$2=="hello" {print $1; exit}')
shome cat "$id"
