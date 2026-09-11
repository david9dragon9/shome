#!/bin/sh
# A resident service: an allocation shome keeps alive.
#
# Batch jobs alone cannot support anything built on top of shome -- reloading a
# 130 GB model per request is not viable. A service holds its allocation across
# requests, while remaining an ordinary scheduled job subject to the same
# limits, owner policy and preemption as everything else.
set -e

cat > /tmp/shome-svc.sh <<'JOB'
#!/bin/sh
echo "service starting on $(hostname), pid $$"
# A real one would start an inference server here, e.g.
#   python -m mlx_lm.server --model $SHOME_MODEL --port 8080
while true; do sleep 5; done
JOB
chmod +x /tmp/shome-svc.sh

echo "--- create ---"
shome serve create demo-svc /tmp/shome-svc.sh --mem 256M
sleep 6
shome serve list

echo
echo "--- it is a normal job in the queue ---"
shome squeue | head -3

echo
echo "--- kill its job; the service brings it back ---"
jid=$(shome serve list | awk '$1=="demo-svc" {print $4}')
shome scancel "$jid"
sleep 14
shome serve list

echo
echo "--- stop means stop ---"
shome serve stop demo-svc
sleep 12
shome serve list

echo
echo "--- clean up ---"
shome serve delete demo-svc
