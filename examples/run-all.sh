#!/bin/sh
# Run every example against a throwaway cluster, then delete it.
#
# Nothing here touches an existing installation: SHOME_ROOT points at a scratch
# directory that is removed at the end, which is also a demonstration that
# uninstalling shome is a directory removal.
set -e

repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"

export SHOME_ROOT="${SHOME_ROOT:-${TMPDIR:-/tmp}/shome-examples}"
# The binary under test is built into the throwaway root too, so this leaves
# nothing behind anywhere -- including no build output in the repository.
export PATH="$SHOME_ROOT/bin:$PATH"
PORT="${SHOME_PORT:-7899}"

cleanup() {
  echo
  echo "=== tearing down ==="
  shome down >/dev/null 2>&1 || true
  rm -rf "$SHOME_ROOT"
  echo "removed $SHOME_ROOT -- that is the whole uninstall"
}
trap cleanup EXIT INT TERM

rm -rf "$SHOME_ROOT"
echo "=== building ==="
mkdir -p "$SHOME_ROOT/bin"
go build -o "$SHOME_ROOT/bin/shome" ./cmd/shome
echo "=== starting a scratch cluster at $SHOME_ROOT ==="
# The console, the login node and the owner's page are all off: their ports
# are fixed, so a throwaway cluster that opened them would fight a real one on
# the same machine -- and `doctor` would rightly report the loser as broken.
# No example uses any of the three.
shome up --port "$PORT" --no-web --no-ssh --no-monitor
shome doctor || true

only="$1"
for ex in examples/0*.sh examples/10-*.py; do
  name=$(basename "$ex")
  case "$name" in 05-*) continue;; esac          # yaml, run via 05 below
  if [ -n "$only" ] && ! echo "$name" | grep -q "$only"; then continue; fi
  echo
  echo "############################################################"
  echo "# $name"
  echo "############################################################"
  case "$ex" in
    *.py) python3 "$ex" ;;
    *)    sh "$ex" ;;
  esac
done

if [ -z "$only" ] || echo "05" | grep -q "$only"; then
  echo
  echo "############################################################"
  echo "# 05-pipeline.yaml"
  echo "############################################################"
  shome pipeline run examples/05-pipeline.yaml
  sleep 12
  shome squeue -a | head -5
fi

echo
echo "=== all examples completed ==="
