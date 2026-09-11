#!/bin/sh
# Submit exactly max-submitted-jobs jobs and watch how many run at once.
# Expect: all accepted; only max-running-jobs RUNNING, the rest PENDING with a
# reason naming the limit. As each finishes, another starts on its own.
#
# Run this one directly.
for i in 1 2 3 4; do
  printf '#!/bin/sh\nsleep 15\n' | shome sbatch --job-name="conc-$i" 2>&1 | tail -1
done
sleep 3
shome squeue -u "${SHOME_ACT_AS:-$USER}"
