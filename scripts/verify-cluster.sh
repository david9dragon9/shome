#!/bin/sh
# Verify a MULTI-MACHINE shome cluster. Run this on the CONTROLLER after at
# least one other machine has joined it with `shome invite`.
#
# It exercises the paths that only exist across a real network: placement onto
# a specific node, isolation there, memory enforcement there, staging over the
# wire, output retrieval from a remote node, and a gang job spanning machines.
#
#   ./scripts/verify-cluster.sh
set -e
command -v shome >/dev/null 2>&1 || { echo "shome is not on your PATH"; exit 1; }

pass=0; fail=0; skip=0
ok()   { printf "  [ ok ] %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  [FAIL] %s\n" "$1"; [ -n "$2" ] && printf "         %s\n" "$2"; fail=$((fail+1)); }
skipd(){ printf "  [skip] %s\n" "$1"; skip=$((skip+1)); }

wait_done() { # wait_done <jobid> <seconds>
  i=0
  while [ $i -lt "${2:-60}" ]; do
    st=$(shome squeue -a | awk -v id="$1" '$1==id {print $4; exit}')
    case "$st" in COMPLETED|FAILED|CANCELLED|TIMEOUT|OUT_OF_MEMORY) echo "$st"; return;; esac
    i=$((i+1)); sleep 1
  done
  echo TIMED_OUT
}
last_id() { shome squeue -a | awk -v n="$1" '$2==n {print $1; exit}'; }

echo "=== cluster ==="
shome sinfo || { echo "controller not reachable"; exit 1; }
nodes=$(shome sinfo | awk 'NR>1 && $2=="UP" && NF>6 {print $1}')
count=$(echo "$nodes" | grep -c . || true)
echo
echo "$count node(s) UP: $(echo $nodes | tr '\n' ' ')"
if [ "$count" -lt 2 ]; then
  echo
  echo "Only one node. Join another with 'shome invite', then re-run."
  exit 1
fi
# The controller's own node is usually named after its hostname, but need not
# be. Fall back to "first node" / "second node" so this works either way.
self=$(echo "$nodes" | grep -x "$(hostname -s)" || echo "$nodes" | head -1)
remote=$(echo "$nodes" | grep -vx "$self" | head -1)
echo "controller node: $self"
echo "remote node:     $remote  <- everything below is pinned here"

echo
echo "=== 1. a job placed on the REMOTE node ==="
cat > /tmp/shome-v-remote.sh <<'EOF'
#!/bin/sh
echo "HOSTNAME=$(hostname)"
echo "NODEID=$SHOME_NODEID/$SHOME_NNODES"
EOF
chmod +x /tmp/shome-v-remote.sh
# Force placement on the remote node by draining this one, rather than by
# --nodelist: draining is exact whatever the cluster looks like, and it
# exercises drain at the same time.
shome admin drain "$self" --reason "verify: forcing placement onto $remote" >/dev/null 2>&1 || true
sleep 2
shome sbatch --job-name vremote /tmp/shome-v-remote.sh >/dev/null
id=$(last_id vremote); st=$(wait_done "$id" 60)
shome admin resume "$self" >/dev/null 2>&1 || true
out=$(shome cat "$id" 2>/dev/null || true)
ranhost=$(echo "$out" | sed -n 's/HOSTNAME=//p')
ranon=$(shome squeue -a | awk -v id="$id" '$1==id {print $(NF-1); exit}')
if [ "$st" = "COMPLETED" ]; then
  ok "job completed on $remote (host reported: ${ranhost:-unknown})"
  echo "$out" | grep -q "NODEID=0/1" && ok "rank env set for a single-node job" \
    || bad "rank env wrong" "$(echo "$out" | grep NODEID)"
else
  bad "job did not complete" "state=$st"
  # Strict FIFO: an unplaceable job blocks everything behind it.
  shome scancel "$id" >/dev/null 2>&1 || true
fi

echo
echo "=== 2. output retrieval across the network ==="
if [ -n "$out" ]; then ok "output fetched from the controller"
else bad "no output archived" "the node may not have uploaded it"; fi

echo
echo "=== 3. isolation ON THE REMOTE NODE ==="
# NOTE: $HOME inside a job points at the job's own scratch by design, so
# testing "$HOME" proves nothing. These probe real targets outside the sandbox.
#
# Listability is NOT the test: bind-mounting the job's scratch necessarily
# materialises its parent directories inside the sandbox, so /home/<user> can
# be listed while containing nothing but the job's own path chain. What matters
# is whether any real content is reachable.
cat > /tmp/shome-v-probe.sh <<'PROBEEOF'
#!/bin/sh
printf 'owner-home='
found=no; leaked=no
for d in /home/* /Users/*; do
  [ -d "$d" ] || continue
  case "$d" in */Shared) continue;; esac
  found=yes
  # Anything that is not shome's own state directory is a real leak.
  for e in "$d"/* "$d"/.[!.]*; do
    [ -e "$e" ] || continue
    case "$e" in *".shome"*|*"/shome-"*) continue;; esac
    leaked=yes
  done
done
if [ "$found" = no ]; then echo none-present
elif [ "$leaked" = yes ]; then echo BAD
else echo denied; fi

# The node agent's private key is the most valuable thing on the machine.
printf 'agent-key='
k=""
for c in /home/*/.shome-node/agent/* /Users/*/.shome*/agent/*; do
  [ -f "$c" ] && k="$c" && break
done
if [ -z "$k" ]; then echo not-reachable
elif head -c 1 "$k" >/dev/null 2>&1; then echo BAD
else echo denied; fi

printf 'net='; curl -s -m 3 https://example.com >/dev/null 2>&1 && echo BAD || echo blocked
printf 'etc-write='; touch /etc/shome-escape 2>/dev/null && echo BAD || echo read-only
printf 'scratch='; touch ./x 2>/dev/null && echo writable || echo BAD
PROBEEOF
chmod +x /tmp/shome-v-probe.sh
shome sbatch --job-name vprobe --nodelist "$remote" /tmp/shome-v-probe.sh >/dev/null
id=$(last_id vprobe); wait_done "$id" 60 >/dev/null
p=$(shome cat "$id" 2>/dev/null || true)
case "$p" in
  *owner-home=denied*)        ok "no real user files reachable" ;;
  *owner-home=none-present*)  skipd "no user home directories on that node to test" ;;
  *)                          bad "a user home directory was READABLE" "$p" ;;
esac
case "$p" in
  *agent-key=denied*)        ok "node agent's private key denied" ;;
  *agent-key=not-reachable*) ok "node agent's key not even visible" ;;
  *)                     bad "the node agent's PRIVATE KEY was readable" "$p" ;;
esac
echo "$p" | grep -q "net=blocked"      && ok "network denied"        || bad "network REACHABLE" "$p"
echo "$p" | grep -q "etc-write=read-only" && ok "/etc is read-only"  || bad "/etc WRITABLE" "$p"
echo "$p" | grep -q "scratch=writable" && ok "own scratch writable"  || bad "scratch not writable" "$p"

echo
echo "=== 4. memory limit on the remote node, from a forked child ==="
cat > /tmp/shome-v-hog.sh <<'EOF'
#!/bin/sh
python3 -c "
x=bytearray(400*1024*1024)
for i in range(0,len(x),4096): x[i]=1
import time; time.sleep(30)" &
wait
EOF
chmod +x /tmp/shome-v-hog.sh
if shome sbatch --job-name vhog --nodelist "$remote" --mem 150M /tmp/shome-v-hog.sh >/dev/null 2>&1; then
  id=$(last_id vhog); st=$(wait_done "$id" 60)
  [ "$st" = "OUT_OF_MEMORY" ] && ok "forked child killed at the limit" \
    || bad "memory limit not enforced" "state=$st (python3 present on that node?)"
else
  skipd "could not submit the memory test"
fi

echo
echo "=== 5. staging, over the wire ==="
w=$(mktemp -d); mkdir -p "$w/input"; echo "a,1" > "$w/input/d.csv"; echo "b,2" >> "$w/input/d.csv"
# Deliberately uses only shell builtins: the point of this test is staging,
# not whether awk happens to be installed on the remote node.
cat > "$w/st.sh" <<'EOF'
#!/bin/sh
mkdir -p results
sum=0
while IFS=, read -r _ n; do sum=$((sum + n)); done < input/d.csv
echo "sum: $sum" > results/out.txt
echo staged
EOF
chmod +x "$w/st.sh"
( cd "$w" && shome sbatch --job-name vstage --nodelist "$remote" --stage-in input --stage-out results st.sh >/dev/null )
id=$(last_id vstage); st=$(wait_done "$id" 90)
if [ "$st" = "COMPLETED" ]; then
  ( cd "$w" && rm -rf results && shome fetch "$id" . >/dev/null 2>&1 )
  if [ -f "$w/results/out.txt" ] && grep -q "sum: 3" "$w/results/out.txt"; then
    ok "inputs went out and results came back"
  else
    bad "results did not come back" "expected results/out.txt containing 'sum: 3'"
  fi
else
  bad "staging job did not complete" "state=$st"
fi
rm -rf "$w"

echo
echo "=== 6. a gang job spanning machines ==="
total=0
for n in $nodes; do
  c=$(shome sinfo | awk -v x="$n" '$1==x {split($6,a,"/"); print a[2]}')
  total=$((total + ${c:-0}))
done
want=$(( total - 1 ))   # more than any one machine has, but the cluster does
cat > /tmp/shome-v-gang.sh <<'EOF'
#!/bin/sh
echo "rank $SHOME_NODEID/$SHOME_NNODES host=$(hostname) nodes=$SHOME_NODELIST"
EOF
chmod +x /tmp/shome-v-gang.sh
if shome sbatch --job-name vgang --total-cpus "$want" /tmp/shome-v-gang.sh >/dev/null 2>&1; then
  id=$(last_id vgang); st=$(wait_done "$id" 120)
  g=$(shome cat "$id" 2>/dev/null || true)
  hosts=$(echo "$g" | sed -n 's/.*host=\([^ ]*\).*/\1/p' | sort -u | grep -c . || echo 0)
  ranks=$(echo "$g" | grep -c '^rank ' || echo 0)
  nlist=$(echo "$g" | sed -n 's/.*nodes=\([^ ]*\).*/\1/p' | head -1)
  nnodes=$(echo "$nlist" | tr ',' '\n' | grep -c . || echo 0)
  if [ "$st" = "COMPLETED" ] && [ "$ranks" -ge 2 ] && [ "$nnodes" -ge 2 ]; then
    ok "$ranks ranks across $nnodes shome nodes ($nlist)"
    if [ "$hosts" -lt 2 ]; then
      printf "         note: only %d distinct hostname(s) -- those nodes share a\n" "$hosts"
      printf "         machine, so this did not cross the network. Real hardware\n"
      printf "         is needed to test that.\n"
    else
      ok "ranks ran on $hosts distinct physical machines"
    fi
  else
    bad "gang job did not span nodes" "state=$st ranks=$ranks nodes=$nnodes"
    echo "$g" | sed 's/^/         /'
  fi
else
  skipd "could not submit a gang job for $want CPUs"
fi

echo
echo "=== 7. draining the remote node ==="
if shome admin drain "$remote" --reason "verify" >/dev/null 2>&1; then
  sleep 3
  s=$(shome sinfo | awk -v n="$remote" '$1==n && NF>6 {print $2; exit}')
  [ "$s" = "DRAIN" ] && ok "$remote drained" || bad "drain did not take" "state=$s"
  shome admin resume "$remote" >/dev/null 2>&1
  sleep 3
  s=$(shome sinfo | awk -v n="$remote" '$1==n && NF>6 {print $2; exit}')
  [ "$s" = "UP" ] && ok "$remote resumed" || bad "resume did not take" "state=$s"
else
  skipd "drain/resume"
fi

echo
echo "============================================"
printf "  %d passed, %d failed, %d skipped\n" "$pass" "$fail" "$skip"
echo "============================================"
if [ "$fail" -gt 0 ]; then
  echo
  echo "For anything that failed, useful context:"
  echo "  shome sinfo && shome squeue -a && shome admin audit -n 20"
  echo "  plus 'shome logs' on the affected machine"
  exit 1
fi
echo
echo "Still worth doing by hand (this script cannot):"
echo "  - turn off Wi-Fi on $remote mid-job; it should go DOWN, and its jobs"
echo "    should stay RUNNING until the grace period expires"
echo "  - turn it back on; it should return to UP on its own"
echo "  (./scripts/watch-node.sh $remote prints every state change)"
