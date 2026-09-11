#!/bin/sh
#SBATCH --job-name=qos-time
#SBATCH --time=4:00:00
# Expect: refused at submission, naming max-walltime.
echo "if you see this, the walltime ceiling was not enforced"
