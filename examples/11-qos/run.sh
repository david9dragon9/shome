#!/bin/sh
# Run every QoS example against a test account and report what happened.
#
#   ./examples/11-qos/run.sh            # creates and uses account 'qtest'
#   ./examples/11-qos/run.sh alice      # uses an existing account
#
# Must be run on the controller, as its owner.
set -e
user="${1:-qtest}"

if [ "$user" = "qtest" ]; then
  shome admin user del qtest >/dev/null 2>&1 || true
  shome admin user add qtest \
    --max-submitted-jobs 4 --max-running-jobs 2 \
    --max-cpus-per-job 2 --max-mem-per-job 512M --max-gpus-per-job 0 \
    --max-walltime 30s --max-procs-per-job 5 --max-disk 10M >/dev/null
  echo "created qtest with tight limits"
fi
echo
shome admin qos show "$user" | sed -n '1,16p'

export SHOME_ACT_AS="$user"
here=$(cd "$(dirname "$0")" && pwd)

say() { printf '\n\033[1m%s\033[0m\n' "$1"; }
submit() { shome sbatch "$here/$1" 2>&1 | tail -1 | sed 's/^/    /'; }

say "refused at submission — the answer is knowable before the job starts"
for f in 01-too-many-cpus.sh 02-too-much-memory.sh 03-too-long.sh 04-forbidden-gpu.sh; do
  printf '  %s\n' "$f"; submit "$f"
done

say "accepted, then killed — only the running job can be measured"
for f in 05-fork-bomb.sh 06-memory-hog.sh; do
  printf '  %s\n' "$f"; submit "$f"
done
printf '  waiting for the supervisor to catch them... '
# Long enough on a busy machine. The memory hog has to allocate its way past
# the limit before anything can notice, and on a loaded Mac that took longer
# than the twelve seconds this used to wait -- so the example reported the
# job as still RUNNING and looked like the limit had not fired.
sleep 25; echo done
shome squeue -u "$user" -a | head -4 | sed 's/^/    /'

say "queue depth"
sh "$here/07-queue-flood.sh" 2>&1 | sed 's/^/    /'

say "concurrency"
sleep 2
shome squeue -u "$user" | head -6 | sed 's/^/    /'

say "disk"
sh "$here/09-disk-fill.sh" 2>&1 | sed 's/^/    /'

say "cleanup"
for id in $(shome squeue -u "$user" 2>/dev/null | awk 'NR>1{print $1}'); do
  shome scancel "$id" >/dev/null 2>&1 || true
done
echo "  cancelled $user's remaining jobs"
echo "  remove the test account with: shome admin user del $user"
