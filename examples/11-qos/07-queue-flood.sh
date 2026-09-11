#!/bin/sh
# Submit more jobs than max-submitted-jobs allows.
# Expect: the first few accepted, then a refusal naming the limit.
#
# Run this one directly; it submits rather than being submitted.
for i in $(seq 1 10); do
  printf '#!/bin/sh\nsleep 20\n' | shome sbatch --job-name="flood-$i" 2>&1 | tail -1
done
