#!/bin/sh
# Print a line whenever a node's state changes. For watching a machine drop off
# the network and come back.
#
#   ./scripts/watch-node.sh <node-name>
#
# Ctrl-C to stop.
node="${1:-}"
[ -z "$node" ] && { echo "usage: $0 <node-name>"; exit 1; }

command -v shome >/dev/null 2>&1 || { echo "shome is not on your PATH"; exit 1; }

prev=""
printf '%s  watching %s (Ctrl-C to stop)\n' "$(date +%H:%M:%S)" "$node"
while true; do
  # Match the table row only: its second field is the state. The per-node
  # "<name> enforcement:" summary line below the table also begins with the
  # node name, so filtering on the name alone matches twice.
  # The SEEN column is two fields, "<n> ago"; REASON is whatever follows it.
  # Anchoring on the literal "ago" field is the only reliable way to split
  # them, because REASON is often empty and sometimes several words.
  line=$(shome sinfo 2>/dev/null | awk -v n="$node" '
    $1==n && ($2=="UP" || $2=="DOWN" || $2=="DRAIN") {
      state=$2; seen="?"; reason=""
      for (i=1; i<=NF; i++) {
        if ($i == "ago") {
          seen=$(i-1) " ago"
          for (j=i+1; j<=NF; j++) reason = reason (reason=="" ? "" : " ") $j
          break
        }
      }
      print state "|" seen "|" reason
      exit
    }')
  if [ -z "$line" ]; then
    cur="ABSENT|not in sinfo|"
  else
    cur="$line"
  fi
  state=$(echo "$cur" | cut -d'|' -f1)
  seen=$(echo "$cur" | cut -d'|' -f2)
  reason=$(echo "$cur" | cut -d'|' -f3)
  # Compare on state and reason only. The "last seen" figure ticks upward every
  # poll, so including it would make every sample look like a change and bury
  # the transitions this is meant to surface.
  key="$state|$reason"
  if [ "$key" != "$prev" ]; then
    printf '%s  %-7s last seen %-10s %s\n' "$(date +%H:%M:%S)" "$state" "$seen" "$reason"
    prev="$key"
  fi
  sleep 3
done
