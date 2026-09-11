#!/bin/sh
# A parameter sweep. Each task gets its own index, exactly as in Slurm.
set -e

cat > /tmp/shome-sweep.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=sweep
#SBATCH --mem=128M
# SHOME_ARRAY_TASK_ID and SLURM_ARRAY_TASK_ID are both set.
lr=$(awk -v i="$SHOME_ARRAY_TASK_ID" 'BEGIN{printf "%.4f", 0.001 * (2 ^ i)}')
echo "task $SHOME_ARRAY_TASK_ID: training with lr=$lr"
sleep 1
JOB
chmod +x /tmp/shome-sweep.sh

echo "--- submit 4 tasks ---"
shome sbatch --array 0-3 /tmp/shome-sweep.sh

sleep 8
echo "--- results ---"
shome squeue -a | head -6
echo
for id in $(shome squeue -a | awk '$2=="sweep" {print $1}'); do
  shome cat "$id" 2>/dev/null | sed 's/^/  /'
done
