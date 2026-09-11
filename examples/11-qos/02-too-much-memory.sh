#!/bin/sh
#SBATCH --job-name=qos-mem
#SBATCH --mem=4G
# Expect: refused at submission, naming max-mem-per-job.
echo "if you see this, the memory ceiling was not enforced"
