#!/bin/sh
#SBATCH --job-name=qos-forks
# Expect: STARTS, then is killed for exceeding max-procs-per-job.
#
# Bounded on purpose. A real fork bomb would take the machine down before the
# supervisor could sample it, and the point here is to see the limit work --
# not to test what happens when it cannot.
i=0
while [ $i -lt 40 ]; do
  sleep 30 &
  i=$((i + 1))
done
wait
