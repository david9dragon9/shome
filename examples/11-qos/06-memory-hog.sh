#!/bin/sh
#SBATCH --job-name=qos-hog
#SBATCH --mem=256M
# Expect: STARTS, then OUT_OF_MEMORY.
#
# The allocation happens in a child process, which is the case that matters:
# macOS cannot inherit a kernel memory cap across fork, so this is caught
# either by shome polling the process tree or, when the spike is faster than
# the poll, by reading what the kernel recorded afterwards.
#
# Deliberately written in the shell rather than in Python: a job's container
# has no interpreter of its own -- Python comes from uv, into the account's
# own directory -- so a hog written in Python quietly skipped itself here and
# the example reported COMPLETED as though the limit had held.
#
# A shell variable doubled is a real allocation, and `wait` after a
# background child is the case that used to report success.
sh -c '
s=$(head -c 1000000 /dev/zero | tr "\0" "x")
i=0
while [ $i -lt 12 ]; do
  s="$s$s"
  i=$((i+1))
  echo "the child now holds twice what it did"
done
sleep 30
' &
wait
echo "the script itself finished normally -- read the job state, not this"
