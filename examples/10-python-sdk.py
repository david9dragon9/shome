#!/usr/bin/env python3
"""Driving shome from Python.

The SDK is not privileged: it can do exactly what the caller's token permits,
so anything you build on shome is just another API client.

    pip install ./sdk/python      # or: PYTHONPATH=sdk/python
"""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk", "python"))
import shome  # noqa: E402

c = shome.Client()  # finds the local socket and token the way the CLI does

print("connected as:", c.whoami())

print("\nnodes:")
for n in c.nodes():
    print(f"  {n.name:12} {n.state:6} {n.used_cpus}/{n.cpus} cpus, {n.mem_mib} MiB, {n.gpus} gpu")

# Ask before you commit: plan() never submits anything.
plan = c.plan(total_cpus=4)
print(f"\nplan: feasible={plan['feasible']} - {plan['why']}")

script = "/tmp/shome-sdk-job.sh"
with open(script, "w") as f:
    f.write("#!/bin/sh\necho 'hello from the SDK'\nsleep 2\necho done\n")
os.chmod(script, 0o755)

job = c.submit(script, name="sdk-job", cpus=1, mem="256M")
print(f"\nsubmitted job {job.id}; waiting...")
done = c.wait(job.id, timeout=120)
print(f"  {done.state} (exit {done.exit_code}) in {done.elapsed}")
print("  output:", c.output(job.id).strip().replace("\n", " | "))

if not done.ok:
    sys.exit(f"job failed: {done.reason}")

print("\nstorage:", c.quota())
print("services:", [(s.name, s.job_state) for s in c.services()] or "none")
