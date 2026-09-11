"""Python client for shome.

Talks to the controller's versioned HTTP API (/api/v1) over the same unix
socket the CLI uses, or over TCP when the admin console is exposed. Nothing
here is privileged: the SDK can do exactly what the caller's token permits,
which is the point -- an application built on shome is just another API client.

    import shome
    c = shome.Client()
    job = c.submit("train.sh", cpus=4, mem="8G")
    print(c.wait(job.id).state)
"""
from __future__ import annotations

import base64
import http.client
import json
import os
import socket
import time
from dataclasses import dataclass, field
from typing import Any, Iterable

__all__ = ["Client", "Job", "Node", "Service", "ShomeError"]

DEFAULT_TIMEOUT = 30.0


class ShomeError(RuntimeError):
    """An error reported by the controller, or a failure reaching it."""


class _UnixConnection(http.client.HTTPConnection):
    """HTTPConnection over a unix socket.

    The controller's client API listens on a unix socket, so filesystem
    permissions gate access. http.client has no unix transport, hence this.
    """

    def __init__(self, path: str, timeout: float = DEFAULT_TIMEOUT):
        super().__init__("localhost", timeout=timeout)
        self._path = path

    def connect(self) -> None:
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.timeout)
        s.connect(self._path)
        self.sock = s


@dataclass
class Job:
    id: int
    name: str = ""
    user: str = ""
    state: str = ""
    reason: str = ""
    node: str = ""
    exit_code: int = 0
    elapsed: str = ""
    raw: dict[str, Any] = field(default_factory=dict)

    @property
    def finished(self) -> bool:
        return self.state in {"COMPLETED", "FAILED", "CANCELLED", "TIMEOUT", "OUT_OF_MEMORY"}

    @property
    def ok(self) -> bool:
        return self.state == "COMPLETED"


@dataclass
class Node:
    name: str
    state: str = ""
    tier: str = ""
    cpus: int = 0
    used_cpus: int = 0
    mem_mib: int = 0
    gpus: int = 0
    raw: dict[str, Any] = field(default_factory=dict)


@dataclass
class Service:
    name: str
    owner: str = ""
    desired: str = ""
    job_id: int = 0
    job_state: str = ""
    endpoint: str = ""
    restarts: int = 0
    raw: dict[str, Any] = field(default_factory=dict)


def _default_root() -> str:
    if v := os.environ.get("SHOME_ROOT"):
        return v
    shared = f"/Users/Shared/shome-{os.environ.get('USER', 'default')}"
    if os.path.isdir(shared):
        return shared
    if v := os.environ.get("XDG_STATE_HOME"):
        return os.path.join(v, "shome")
    return os.path.join("/tmp", "shome")


def _default_token(root: str) -> str:
    if v := os.environ.get("SHOME_TOKEN"):
        return v.strip()
    for path in (
        os.path.expanduser("~/.shome/token"),
        os.path.join(root, "admin.token"),
    ):
        try:
            with open(path) as f:
                return f.read().strip()
        except OSError:
            continue
    return ""


class Client:
    """A shome API client.

    By default it finds the local controller and token the same way the CLI
    does, so a script on the controller host needs no configuration.
    """

    def __init__(
        self,
        socket_path: str | None = None,
        url: str | None = None,
        token: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
    ):
        root = _default_root()
        self._url = url
        self._socket = socket_path or (None if url else os.path.join(root, "shome.sock"))
        self._token = token if token is not None else _default_token(root)
        self._timeout = timeout
        if not self._token:
            raise ShomeError(
                "no API token: set SHOME_TOKEN, write ~/.shome/token, "
                "or pass token= explicitly"
            )

    # ---- transport -------------------------------------------------

    def _conn(self):
        if self._url:
            host = self._url.split("://", 1)[-1]
            return http.client.HTTPConnection(host, timeout=self._timeout)
        return _UnixConnection(self._socket, timeout=self._timeout)

    def _request(self, method: str, path: str, body: Any = None) -> Any:
        prefix = "/api/v1" if self._url else ""
        payload = json.dumps(body) if body is not None else None
        headers = {"Authorization": f"Bearer {self._token}"}
        if payload is not None:
            headers["Content-Type"] = "application/json"
        conn = self._conn()
        try:
            conn.request(method, prefix + path, body=payload, headers=headers)
            resp = conn.getresponse()
            raw = resp.read()
            if resp.status >= 400:
                try:
                    msg = json.loads(raw).get("error", raw.decode(errors="replace"))
                except Exception:
                    msg = raw.decode(errors="replace")
                raise ShomeError(f"{method} {path}: {msg}")
            if not raw:
                return None
            try:
                return json.loads(raw)
            except json.JSONDecodeError:
                return raw.decode(errors="replace")
        except (OSError, http.client.HTTPException) as e:
            raise ShomeError(f"cannot reach the shome controller: {e}") from e
        finally:
            conn.close()

    # ---- identity --------------------------------------------------

    def whoami(self) -> dict[str, Any]:
        return self._request("GET", "/whoami")

    # ---- jobs ------------------------------------------------------

    def submit(
        self,
        script: str,
        *,
        name: str | None = None,
        cpus: int = 1,
        mem: str | None = None,
        gpus: int = 0,
        time_limit: str | None = None,
        constraint: str | None = None,
        total_cpus: int = 0,
        total_gpu_mem: str | None = None,
        fabric: str | None = None,
        model: str | None = None,
        network: bool = False,
        env: dict[str, str] | None = None,
        args: Iterable[str] = (),
    ) -> Job:
        """Submit a batch job. Mirrors the sbatch options."""
        # Send the script body, not just its path: the job runs a copy in its
        # own scratch, so a script in your home directory (which the sandbox
        # denies) or on a machine the node cannot see still works.
        with open(script, "rb") as f:
            body = f.read()
        spec: dict[str, Any] = {
            "Name": name or os.path.basename(script),
            "Script": os.path.abspath(script),
            "ScriptBody": base64.b64encode(body).decode(),
            "Args": list(args),
            "Env": env or {},
            "ArrayTaskID": -1,
            "Limits": {"CPUs": cpus, "GPUs": gpus, "Network": network},
        }
        if mem:
            spec["Limits"]["MemBytes"] = _parse_size(mem)
        if time_limit:
            spec["Limits"]["Walltime"] = _parse_duration_ns(time_limit)
        if constraint:
            spec["Constraint"] = constraint
        if total_cpus:
            spec["TotalCPUs"] = total_cpus
        if total_gpu_mem:
            spec["TotalGPUMem"] = _parse_size(total_gpu_mem)
        if fabric:
            spec["Fabric"] = fabric
        if model:
            spec["Model"] = model
        # SubmitRequest embeds job.Spec, so Go inlines the spec fields at the
        # top level rather than nesting them under a "Spec" key.
        return _job(self._request("POST", "/submit", spec))

    def job(self, job_id: int) -> Job:
        return _job(self._request("GET", f"/job/{job_id}"))

    def jobs(self, *, active_only: bool = True) -> list[Job]:
        q = "/jobs?active=1" if active_only else "/jobs"
        return [_job(j) for j in self._request("GET", q) or []]

    def cancel(self, job_id: int) -> None:
        self._request("POST", f"/cancel/{job_id}")

    def output(self, job_id: int) -> str:
        out = self._request("GET", f"/job/{job_id}/output")
        return out if isinstance(out, str) else json.dumps(out)

    def wait(self, job_id: int, *, poll: float = 1.0, timeout: float | None = None) -> Job:
        """Block until a job reaches a terminal state."""
        deadline = None if timeout is None else time.monotonic() + timeout
        while True:
            j = self.job(job_id)
            if j.finished:
                return j
            if deadline is not None and time.monotonic() > deadline:
                raise TimeoutError(f"job {job_id} still {j.state} after {timeout}s")
            time.sleep(poll)

    # ---- capacity --------------------------------------------------

    def nodes(self) -> list[Node]:
        return [
            Node(
                name=n.get("name", ""), state=n.get("state", ""), tier=n.get("tier", ""),
                cpus=n.get("cpus", 0), used_cpus=n.get("used_cpus", 0),
                mem_mib=n.get("mem_mib", 0), gpus=n.get("gpus", 0), raw=n,
            )
            for n in self._request("GET", "/nodes") or []
        ]

    def plan(self, **kwargs) -> dict[str, Any]:
        """Ask whether a request could be scheduled, without submitting it."""
        spec: dict[str, Any] = {"ArrayTaskID": -1}
        if v := kwargs.get("total_cpus"):
            spec["TotalCPUs"] = v
        if v := kwargs.get("total_mem"):
            spec["TotalMemBytes"] = _parse_size(v)
        if v := kwargs.get("total_gpu_mem"):
            spec["TotalGPUMem"] = _parse_size(v)
        if v := kwargs.get("constraint"):
            spec["Constraint"] = v
        if v := kwargs.get("model"):
            spec["Model"] = v
        return self._request("POST", "/plan", spec)

    # ---- services --------------------------------------------------

    def services(self) -> list[Service]:
        return [
            Service(
                name=s.get("name", ""), owner=s.get("owner", ""),
                desired=s.get("desired", ""), job_id=s.get("job_id", 0),
                job_state=s.get("job_state", ""), endpoint=s.get("endpoint", ""),
                restarts=s.get("restarts", 0), raw=s,
            )
            for s in self._request("GET", "/services") or []
        ]

    def create_service(self, name: str, script: str, **kwargs) -> None:
        spec: dict[str, Any] = {
            "Name": name,
            "Script": os.path.abspath(script),
            "ScriptBody": base64.b64encode(open(script, "rb").read()).decode(),
            "ArrayTaskID": -1,
            "Limits": {
                "CPUs": kwargs.get("cpus", 1),
                "GPUs": kwargs.get("gpus", 0),
            },
        }
        if mem := kwargs.get("mem"):
            spec["Limits"]["MemBytes"] = _parse_size(mem)
        if v := kwargs.get("total_gpu_mem"):
            spec["TotalGPUMem"] = _parse_size(v)
        if v := kwargs.get("model"):
            spec["Model"] = v
        self._request("POST", "/services", {
            "name": name, "spec": spec,
            "idle_timeout": kwargs.get("idle_timeout", ""),
        })

    def delete_service(self, name: str) -> None:
        self._request("DELETE", f"/services/{name}")

    # ---- usage and limits -------------------------------------------

    def quota(self) -> dict[str, Any]:
        """Everything this account is using and allowed.

        Compute in use against its ceilings, storage on each machine against
        the cluster-wide limit, and fair-share standing. The same figures
        `shome quota` prints.

        This used to return only the controller's storage; under the
        per-machine storage model that was one machine's share of the answer.
        """
        return self._request("GET", "/quota")

    def storage_usage(self) -> dict[str, Any]:
        """Storage per machine, and the total the limit applies to."""
        return self._request("GET", "/fs/usage")


_UNITS = {"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}


def _parse_size(s: str | int) -> int:
    """Parse '4G' / '512M' / 1024. A bare number is MiB, matching Slurm."""
    if isinstance(s, int):
        return s << 20
    s = s.strip()
    if not s:
        return 0
    mult = 1 << 20
    if s[-1].upper() in _UNITS:
        mult, s = _UNITS[s[-1].upper()], s[:-1]
    return int(float(s) * mult)


def _parse_duration_ns(s: str) -> int:
    """Parse Slurm time forms: MM, MM:SS, HH:MM:SS, D-HH:MM:SS."""
    days = 0
    if "-" in s:
        d, s = s.split("-", 1)
        days = int(d)
    parts = [int(p) for p in s.split(":")] if s else [0]
    if len(parts) == 1:
        h, m, sec = (parts[0], 0, 0) if days else (0, parts[0], 0)
    elif len(parts) == 2:
        h, m, sec = (parts[0], parts[1], 0) if days else (0, parts[0], parts[1])
    else:
        h, m, sec = parts[0], parts[1], parts[2]
    return ((days * 86400) + h * 3600 + m * 60 + sec) * 1_000_000_000


def _job(d: dict[str, Any] | None) -> Job:
    d = d or {}
    return Job(
        id=d.get("id", 0), name=d.get("name", ""), user=d.get("user", ""),
        state=d.get("state", ""), reason=d.get("reason", ""), node=d.get("node", ""),
        exit_code=d.get("exit_code", 0), elapsed=d.get("elapsed", ""), raw=d,
    )
