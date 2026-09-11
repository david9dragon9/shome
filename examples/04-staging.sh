#!/bin/sh
# Getting data in and results out.
#
# shome stages explicitly rather than assuming a shared filesystem: a home
# cluster rarely has one, and a job that pulls from a NAS over Wi-Fi is slow
# in a way that is hard to see.
set -e

work=$(mktemp -d)
cd "$work"

mkdir -p input
printf 'gene_a,3\ngene_b,7\ngene_c,11\n' > input/data.csv
echo "--- local input ---"; cat input/data.csv

cat > analyse.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=analyse
#SBATCH --mem=256M
mkdir -p results
awk -F, '{s+=$2} END {print "total:", s}' input/data.csv > results/summary.txt
echo "processed $(wc -l < input/data.csv | tr -d ' ') rows"
JOB
chmod +x analyse.sh

echo
echo "--- submit with staging ---"
# --stage-in uploads these paths into the job's working directory.
# --stage-out brings these paths back when it finishes.
shome sbatch --stage-in input --stage-out results analyse.sh

for _ in $(seq 1 30); do
  state=$(shome squeue -a | awk '$2=="analyse" {print $4; exit}')
  case "$state" in COMPLETED|FAILED) break;; esac
  sleep 1
done
id=$(shome squeue -a | awk '$2=="analyse" {print $1; exit}')

echo
echo "--- job output ---"; shome cat "$id"
echo
echo "--- fetch results ---"
rm -rf results
shome fetch "$id" .
cat results/summary.txt
echo
echo "(worked in $work)"
