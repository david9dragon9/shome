#!/bin/sh
#SBATCH --job-name=qos-gpu
#SBATCH --gres=gpu:1
# Expect: refused when the account's max-gpus-per-job is 0.
#
# This is the case that needs zero to mean "none allowed" rather than
# "unlimited": an account that may compute but must not touch accelerators.
echo "if you see this, the accelerator ban was not enforced"
