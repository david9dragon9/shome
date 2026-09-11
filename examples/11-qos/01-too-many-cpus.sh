#!/bin/sh
#SBATCH --job-name=qos-cpus
#SBATCH --cpus-per-task=8
# Expect: refused at submission, naming max-cpus-per-job.
echo "if you see this, the CPU limit was not enforced"
