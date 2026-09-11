#!/bin/sh
# Multi-tenancy: several people sharing one cluster.
#
# Identity comes from the caller's token, never from the request, so nobody can
# submit work billed to someone else.
set -e

echo "=== create accounts ==="
# Checked rather than assumed: a token that comes out empty would not run as
# nobody, it would run as the machine's owner -- who is an administrator. The
# CLI refuses an empty SHOME_TOKEN for that reason, and this stops earlier
# still, where the account was supposed to be created.
mint() {
  name=$1; shift
  tok=$(shome admin user add "$name" "$@" | grep -oE "$name\.[A-Za-z0-9_-]+" | head -1)
  if [ -z "$tok" ]; then
    echo "could not create the account '$name'; the rest of this example would" >&2
    echo "have run as the cluster owner instead, so it stops here." >&2
    exit 1
  fi
  printf '%s' "$tok"
}
alice=$(mint alice --max-disk 1G)
bob=$(mint bob --max-disk 1G)
ops=$(mint ops --role operator)
shome admin user list

cat > /tmp/shome-private.sh <<'JOB'
#!/bin/sh
#SBATCH --job-name=private
echo "secret result"
JOB
chmod +x /tmp/shome-private.sh

echo
echo "=== alice submits ==="
SHOME_TOKEN="$alice" shome sbatch /tmp/shome-private.sh
sleep 4
aid=$(shome squeue -a | awk '$2=="private" {print $1; exit}')

echo
echo "=== bob cannot see or touch it ==="
printf "  read output:  "; SHOME_TOKEN="$bob" shome cat "$aid" 2>&1 | head -1
printf "  cancel:       "; SHOME_TOKEN="$bob" shome scancel "$aid" 2>&1 | head -1
bobjobs=$(SHOME_TOKEN="$bob" shome squeue -a | tail -n +2 | grep -cv '^(no jobs)$' || true)
echo    "  his queue:    ${bobjobs:-0} job(s) -- alice's is not among them"

echo
echo "=== alice can ==="
SHOME_TOKEN="$alice" shome cat "$aid" | sed 's/^/  /'

echo
echo "=== roles ==="
printf "  operator drains a node: "; SHOME_TOKEN="$ops" shome admin drain "$(hostname -s)" 2>&1 | head -1
printf "  operator lists users:   "; SHOME_TOKEN="$ops" shome admin user list 2>&1 | head -1
printf "  alice drains a node:    "; SHOME_TOKEN="$alice" shome admin drain "$(hostname -s)" 2>&1 | head -1
SHOME_TOKEN="$ops" shome admin resume "$(hostname -s)" >/dev/null 2>&1 || true

echo
echo "=== storage is per-user and quota'd ==="
head -c 300000 /dev/urandom > /tmp/shome-blob
SHOME_TOKEN="$alice" shome storage put /tmp/shome-blob data.bin
SHOME_TOKEN="$alice" shome storage du
printf "  bob's view:  "; SHOME_TOKEN="$bob" shome storage ls
printf "  traversal:   "; SHOME_TOKEN="$bob" shome storage get ../alice/data.bin 2>&1 | head -1

echo
echo "=== break-glass ==="
shome admin quarantine bob --reason "example" | head -1
printf "  bob's token now: "; SHOME_TOKEN="$bob" shome squeue 2>&1 | head -1
shome admin unquarantine bob >/dev/null
