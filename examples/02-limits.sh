#!/bin/sh
# Resource limits, and what shome does when a job exceeds them.
#
# The memory case is the interesting one: the limit catches a FORKED CHILD,
# which a plain kernel cap on the launched process would miss entirely.
set -e

wait_for() {  # wait_for <job-name>
  for _ in $(seq 1 40); do
    state=$(shome squeue -a | awk -v n="$1" '$2==n {print $4; exit}')
    case "$state" in COMPLETED|FAILED|CANCELLED|TIMEOUT|OUT_OF_MEMORY) echo "$state"; return;; esac
    sleep 1
  done
  echo TIMEOUT_WAITING
}

echo "=== 1. a job that fits ==="
cat > /tmp/shome-fits.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=fits
#SBATCH --cpus-per-task=2
#SBATCH --mem=256M
echo "given 2 cpus and 256 MiB"
JOB
chmod +x /tmp/shome-fits.sh
shome sbatch /tmp/shome-fits.sh
echo "  -> $(wait_for fits)"

echo
echo "=== 2. exceeding --mem, from a forked child ==="
cat > /tmp/shome-hog.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=memhog
#SBATCH --mem=150M
# The allocator runs in a CHILD of this shell. shome's supervisor watches the
# whole process tree, so the limit still applies.
python3 -c "
x = bytearray(400*1024*1024)
for i in range(0, len(x), 4096): x[i] = 1
import time; time.sleep(30)
" &
wait
JOB
chmod +x /tmp/shome-hog.sh
shome sbatch /tmp/shome-hog.sh
echo "  -> $(wait_for memhog)"
shome squeue -a | awk '$2=="memhog" {print "     " $NF}' | head -1
shome squeue -a | grep memhog | sed 's/^/     /'

echo
echo "=== 3. exceeding --time ==="
cat > /tmp/shome-slow.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=slowjob
#SBATCH --time=00:00:03
sleep 300
JOB
chmod +x /tmp/shome-slow.sh
shome sbatch /tmp/shome-slow.sh
echo "  -> $(wait_for slowjob)"

echo
echo "=== 4. asking for more than exists is refused at submit ==="
cat > /tmp/shome-huge.sh <<'JOB'
#!/bin/sh
echo hi
JOB
chmod +x /tmp/shome-huge.sh
shome sbatch --cpus-per-task 9999 /tmp/shome-huge.sh 2>&1 | sed 's/^/  /' || true
