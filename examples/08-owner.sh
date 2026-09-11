#!/bin/sh
# Reclaiming your own machine.
#
# shome's first rule is that the machine's owner outranks the cluster admin.
# Everything here works with no network call, so it still works when the
# controller is unreachable, and it survives a reboot.
set -e

echo "=== what is this machine doing? ==="
shome status

echo
echo "=== write a contribution policy ==="
root="${SHOME_ROOT:-$(shome doctor 2>/dev/null | awk '/state directory/{print $3}')}"
# Caps only. Deliberately NO yield rules here: a rule such as
#   on_battery: {action: suspend}
# is a perfectly good thing to want, but if the machine is on battery right
# now it suspends the node immediately and the rest of this example cannot
# run. Reactive rules are shown separately at the end.
cat > "$root/node.yaml" <<'POLICY'
contribute:
  max_cores: 4          # never use more than 4 cores, whatever is idle
  max_mem_gb: 4
  gpu: exclusive
POLICY
sleep 4

echo "--- capacity now reflects the policy, not the hardware ---"
shome sinfo | head -3

echo
echo "=== start a job, then take the machine back ==="
cat > /tmp/shome-tick.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=ticker
i=0
while [ $i -lt 30 ]; do echo "tick $i"; i=$((i+1)); sleep 1; done
echo DONE
JOB
chmod +x /tmp/shome-tick.sh
shome sbatch /tmp/shome-tick.sh
sleep 5

echo "--- pause: jobs are SUSPENDED, not killed ---"
shome pause "stepping away"
sleep 4
shome status | head -1
shome sinfo | head -2

echo
echo "--- resume: the job continues where it left off ---"
shome resume
sleep 4
shome status | head -1

for _ in $(seq 1 60); do
  state=$(shome squeue -a | awk '$2=="ticker" {print $4; exit}')
  case "$state" in COMPLETED|FAILED|CANCELLED) break;; esac
  sleep 1
done
id=$(shome squeue -a | awk '$2=="ticker" {print $1; exit}')
echo
echo "--- no ticks lost across the pause ---"
ticks=$(shome cat "$id" 2>/dev/null | grep -c '^tick ' || echo 0)
shome cat "$id" 2>/dev/null | head -2
echo "  ..."
shome cat "$id" 2>/dev/null | tail -1
echo "  ticks: $ticks/30 (all 30 means the job resumed rather than restarting)"

echo
echo "=== reactive rules ==="
# These fire on the machine's live state. Shown rather than applied, because
# whether they trigger depends on whether you are plugged in right now.
cat <<'POLICY'
  yield:
    on_user_input:      {within: 30s, action: throttle}
    on_battery:         {action: suspend}
    on_thermal:         {above: serious, action: throttle}
    on_app_running:     {apps: [Xcode], action: suspend}
POLICY
echo "  current signals:"
shome status | sed -n '/^signals/,/^$/p' | sed 's/^/  /'
echo "  (with on_battery set, a machine on battery suspends immediately --"
echo "   correct, but it would stop this example running, so it is not applied here)"

rm -f "$root/node.yaml"
