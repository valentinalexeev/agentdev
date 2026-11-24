"""Toolchain setup of an agent pod (#499): runs in the init container "toolchain" with the worker image's python3.

The operator embeds this file (toolchain.go) and passes the Agent's spec.toolchain as TOOLCHAIN_SPEC (JSON). The steps
install into TOOLCHAIN_DIR (/state/toolchain, the agent's volume). A hash of the steps and path in status.json skips
the setup on a wake when nothing changed, every step passed and the refresh time has not passed yet.

Exit 1 only when a step failed and failurePolicy is Fail: the pod then does not start.
"""
import hashlib
import json
import os
import signal
import subprocess
import sys
import time

DEFAULT_PATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"


def spec_hash(spec):
    key = {"steps": spec.get("steps") or [], "path": spec.get("path") or []}
    return hashlib.sha256(json.dumps(key, sort_keys=True).encode()).hexdigest()


def up_to_date(status, h, refresh, now):
    if not isinstance(status, dict) or status.get("hash") != h:
        return False
    steps = status.get("steps")
    if not isinstance(steps, list) or not all(isinstance(s, dict) and s.get("result") == "ok" for s in steps):
        return False
    if refresh > 0:
        finished = status.get("finished")
        if not isinstance(finished, (int, float)) or now - finished >= refresh:
            return False
    return True


def run_step(step, cwd, env):
    timeout = step.get("timeoutSeconds") or 600
    start = time.monotonic()
    res = {"name": step["name"], "result": "ok", "version": ""}
    try:
        for i, cmd in enumerate([step["run"], step.get("verify")]):
            if not cmd:
                continue
            left = max(1, timeout - (time.monotonic() - start))
            # own process group: on the time limit the whole pipeline goes (sh -c "curl | tar"), not only sh
            p = subprocess.Popen(cmd, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                 stderr=subprocess.STDOUT, start_new_session=True)
            try:
                out, _ = p.communicate(timeout=left)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)
                p.communicate()
                raise
            out = out.decode(errors="replace")
            sys.stdout.write(out)
            if p.returncode != 0:
                res["result"] = "failed"
                res["error"] = "%s exited %d" % ("verify" if i else "run", p.returncode)
                break
            if i:
                res["version"] = (out.strip().splitlines() or [""])[0][:200]
    except subprocess.TimeoutExpired:
        res["result"], res["error"] = "timeout", "time limit %ds" % timeout
    except OSError as e:
        res["result"], res["error"] = "failed", str(e)
    res["seconds"] = round(time.monotonic() - start, 1)
    return res


def main():
    spec = json.loads(os.environ["TOOLCHAIN_SPEC"])
    root = os.environ.get("TOOLCHAIN_DIR", "/state/toolchain")
    refresh = int(os.environ.get("TOOLCHAIN_REFRESH_SECONDS", "0") or "0")
    fail = spec.get("failurePolicy", "Fail") != "Continue"
    status_file = os.path.join(root, "status.json")
    home, tmp = os.path.join(root, "home"), os.path.join(root, "tmp")
    for d in (root, home, tmp):
        os.makedirs(d, exist_ok=True)

    h, now = spec_hash(spec), time.time()
    try:
        with open(status_file) as f:
            status = json.load(f)
    except (OSError, ValueError):
        status = None
    if up_to_date(status, h, refresh, now):
        print("toolchain: up to date (%s)" % h[:12])
        return 0

    env = {"HOME": home, "TMPDIR": tmp, "TOOLCHAIN_DIR": root,
           "PATH": ":".join((spec.get("path") or []) + [DEFAULT_PATH])}
    for k in ("http_proxy", "https_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"):
        if k in os.environ:
            env[k] = os.environ[k]
    results = []
    for step in spec.get("steps") or []:
        print("toolchain: step %s" % step["name"], flush=True)
        r = run_step(step, root, env)
        results.append(r)
        print("toolchain: step %s %s in %.1fs %s" % (r["name"], r["result"], r["seconds"], r.get("error", r["version"])),
              flush=True)
        if r["result"] != "ok":
            break

    status = {"hash": h, "finished": time.time(), "steps": results}
    with open(status_file + ".tmp", "w") as f:
        json.dump(status, f, indent=1)
    os.replace(status_file + ".tmp", status_file)
    if any(r["result"] != "ok" for r in results):
        print("toolchain: FAILED (failurePolicy %s)" % ("Fail" if fail else "Continue"), flush=True)
        return 1 if fail else 0
    return 0


if __name__ == "__main__":
    sys.exit(main())
