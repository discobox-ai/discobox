#!/usr/bin/env python3
"""Required Docker UX smoke: isolated stack, no skips, bounded subprocesses."""
from concurrent.futures import ThreadPoolExecutor, as_completed
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import sqlite3
import subprocess
import tarfile
import threading
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[2]
ARTIFACTS = ROOT / "build/ux-smoke" / uuid.uuid4().hex[:12]
ARTIFACTS.mkdir(parents=True)
# Never inherit the enclosing box's identity or a developer's server configuration.
ENV = {k: v for k, v in os.environ.items()
       if not k.startswith(("DISCOBOX_", "DATABASE_", "DISPATCHER_"))}
ENV.update({f"DISCOBOX_{name}_DIR": str(ARTIFACTS / name.lower())
            for name in ("DATA", "CONFIG", "CACHE", "STATE")})
ENV["DISCOBOX_ENV_FILE"] = str(ARTIFACTS / "empty.env")
(ARTIFACTS / "empty.env").touch()
ENV["DISCOBOX_CONFIG_FILE"] = ""
# Isolate runtime configuration without throwing away the caller's Go cache.
ENV["GOCACHE"] = subprocess.check_output(["go", "env", "GOCACHE"], text=True).strip()
for name in ("CONFIG", "DATA", "CACHE", "STATE"):
    ENV[f"XDG_{name}_HOME"] = str(ARTIFACTS / ("xdg-" + name.lower()))
RUN = ARTIFACTS.name
BUILDER = "ux-smoke-" + RUN
BASE = "discobox-ux-base:" + RUN
BOXES = []
POOLS = []
SERVER = None
BUILDER_CREATED = False
CLI = []
COMMAND_COUNT = 0
COMMAND_LOCK = threading.Lock()


class CommandFailure(RuntimeError):
    def __init__(self, message, result):
        super().__init__(message)
        self.result = result


def command(args, *, limit=120, expected=0, input=None):
    global COMMAND_COUNT
    started = time.monotonic()
    with COMMAND_LOCK:
        COMMAND_COUNT += 1
        output_path = ARTIFACTS / f"command-{COMMAND_COUNT:04}.log"
        error_path = ARTIFACTS / f"command-{COMMAND_COUNT:04}.stderr.log"
        print("+", " ".join(str(a) if len(str(a)) < 180 else str(a)[:160] + "..." for a in args), flush=True)
    # Write while the command runs so bootstrap failures/timeouts retain output.
    # Progress belongs to stderr; assertions and JSON parsing consume stdout.
    # Keep both files open during execution so timeouts retain both streams.
    with output_path.open("w") as output, error_path.open("w") as errors:
        result = subprocess.run(list(map(str, args)), cwd=ROOT, env=ENV,
                                input=input, stdout=output, stderr=errors,
                                text=True, timeout=limit)
    result.stdout = output_path.read_text()
    result.stderr = error_path.read_text()
    with COMMAND_LOCK, (ARTIFACTS / "commands.log").open("a") as log:
        log.write(f"$ {args!r}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}\nexit={result.returncode} duration={time.monotonic()-started:.2f}s\n")
    if result.returncode != expected:
        raise CommandFailure(f"exit {result.returncode}, expected {expected}: {args}\nstdout:\n{result.stdout}\nstderr:\n{result.stderr}", result)
    return result.stdout


def cli(*args, **kwargs):
    return command(CLI + list(args), **kwargs)


def obj(*args):
    return json.loads(cli(*args))


def api(path, method="GET", body=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(URL + "/projects/default/" + path,
                                 data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as response:
        raw = response.read()
        with (ARTIFACTS / "api.log").open("a") as log:
            log.write(f"{method} {path}: {response.status}\n{raw.decode()}\n")
        return json.loads(raw) if raw else None


def eventually(description, check, seconds=120):
    deadline = time.monotonic() + seconds
    while True:
        value = check()
        if value:
            return value
        if time.monotonic() >= deadline:
            raise RuntimeError("timed out: " + description)
        time.sleep(1)


def bundle(name, dockerfile, context=".", target=None, metadata=None):
    layout = ARTIFACTS / (name + ".tar")
    args = ["docker", "buildx", "build", "--builder", BUILDER,
            "--provenance=false", "-f", dockerfile,
            "--output", f"type=oci,dest={layout}"]
    if target:
        args += ["--target", target]
    if metadata:
        args += ["--build-arg", metadata]
    command(args + [context], limit=900)
    with tarfile.open(layout) as archive:
        index = json.load(archive.extractfile("index.json"))
    return {"reference": f"discobox-ux-{name}:{RUN}",
            "digest": index["manifests"][0]["digest"], "layout": str(layout)}


def build_stack():
    started = time.monotonic()
    # A single Go invocation shares compilation across both binaries. Overlap it
    # with image builds; three workers bound CPU/memory on the four-core CI host.
    # ENV remains read-only until every build has joined. BuildKit still owns
    # stage deduplication and its Dockerfiles' locked Go cache mounts.
    with ThreadPoolExecutor(max_workers=3) as builds:
        prerequisites = [
            builds.submit(command, ["go", "build", "-o", ARTIFACTS,
                                    "./server/cmd/discobox-server", "./cli/cmd/discobox"], limit=300),
            builds.submit(command, ["go", "tool", "task", "build:sandbox-base-image", "BASE_IMAGE=" + BASE], limit=600),
        ]
        bundles = [
            builds.submit(bundle, "pool", "pool-agent/Dockerfile"),
            builds.submit(bundle, "agent", "sandbox-agent/Dockerfile", target="agent"),
            builds.submit(bundle, "platform", "sandbox-agent/Dockerfile", target="platform",
                          metadata="IMAGE_LAYER_METADATA=" + json.dumps(json.loads((ROOT / "sandbox-agent/image.json").read_text()))),
            builds.submit(bundle, "harness", "test/ux-smoke/image/Dockerfile", context="test/ux-smoke/image",
                          metadata="HARNESS_METADATA=" + json.dumps(json.loads((ROOT / "test/ux-smoke/image/image.json").read_text()))),
        ]
        try:
            for build in as_completed(prerequisites + bundles):
                build.result()
        except BaseException:
            for build in prerequisites + bundles:
                build.cancel()
            # The executor joins running builds before cleanup can remove their
            # builder, image tag or output files, including on interruption.
            raise
        result = [build.result() for build in bundles]
    print(f"TIMING builds {time.monotonic()-started:.1f}s", flush=True)
    return result


def create_box(name, **kwargs):
    # Record the ID before waiting so a failed provisioning still has diagnostics.
    args = ["admin", "box", "create", "--name", name, "--harness", "ux-smoke"]
    if kwargs.get("requester"):
        args += ["--discobox-api"]
    box = obj(*args)
    BOXES.append(box["id"])
    (ARTIFACTS / "boxes.json").write_text(json.dumps(BOXES))
    wait_box(box["id"])
    return box["id"]


def wait_box(box_id):
    eventually("running " + box_id,
               lambda: obj("admin", "box", "get", box_id)["runtime"].get("displayState") == "running",
               seconds=180)
    eventually("primary terminal launched " + box_id, lambda: any(t.get("primary") and t["status"] == "running"
               for t in (obj("admin", "terminal", "ls", box_id).get("terminals") or [])), seconds=120)


def start_server():
    global SERVER
    with (ARTIFACTS / "server.log").open("a") as log:
        SERVER = subprocess.Popen([str(ARTIFACTS / "discobox-server")], cwd=ROOT, env=ENV,
                                  stdout=log, stderr=subprocess.STDOUT, start_new_session=True)

    def ready():
        if SERVER.poll() is not None:
            raise RuntimeError("server exited; see server.log")
        try:
            with urllib.request.urlopen(URL + "/openapi.yaml", timeout=2):
                return True
        except OSError:
            return False
    eventually("server ready", ready, seconds=30)


def wait_settled(label, seconds=330):
    """Wait for the server to report ready with no pool still upgrading.

    After an upgrade or restart the server serves at once and reports
    upgrading_pools (503) until every pool it had is on its agent, then ready;
    a pool it gave up on stays counted on a ready answer. Nothing here uses a
    box before that, and the time it took is its own number.
    """
    started = time.time()
    last = {}

    def settled():
        nonlocal last
        try:
            with urllib.request.urlopen(URL + "/healthz", timeout=2) as response:
                last = json.load(response)
        except urllib.error.HTTPError as error:
            last = json.load(error)
            return False
        except OSError:
            return False
        return last.get("status") == "ready" and not last.get("poolsUpgrading")
    try:
        eventually(label, settled, seconds=seconds)
    finally:
        (ARTIFACTS / "health-settled.json").write_text(json.dumps(last, indent=2))
    elapsed = time.time() - started
    print(f"TIME {label}: {elapsed:.1f}s", flush=True)
    return elapsed


def restart_control_plane(box):
    pool = obj("admin", "box", "get", box)["poolId"]
    container = "discobox-vm-" + pool
    identity_args = ["docker", "exec", container, "systemctl", "show", "discobox-pool-agent",
                     "--property=MainPID,InvocationID,ActiveEnterTimestampMonotonic"]
    before = command(identity_args)
    (ARTIFACTS / "pool-agent-before-restart.log").write_text(before)
    command(["docker", "exec", container, "sh", "-c",
             'sha256sum /proc/$(systemctl show -p MainPID --value discobox-pool-agent)/exe'])
    # Keep the nonce only in the primary shell's memory. Terminal IDs alone
    # survive a relaunch, so cannot prove that its program survived.
    cli("send", box, f"export RESTART_SMOKE_TOKEN={RUN}; printf 'restart-armed:%s\\n' \"$RESTART_SMOKE_TOKEN\"")
    cli("send-key", box, "Enter")
    eventually("primary armed for restart", lambda: "restart-armed:" + RUN in
               "\n".join(obj("capture", box)["lines"]), seconds=15)
    stopped_at = time.time()
    os.killpg(SERVER.pid, signal.SIGTERM)
    SERVER.wait(timeout=30)
    # Leave a full heartbeat interval with no server, exercising a failed
    # report before recovery rather than a restart between two successful beats.
    time.sleep(35)
    failed_beat = command(["docker", "exec", container, "journalctl", "-u", "discobox-pool-agent",
                           "--no-pager", "--since", f"@{stopped_at:.6f}"])
    (ARTIFACTS / "pool-failed-heartbeat.log").write_text(failed_beat)
    assert "report pool status" in failed_beat, "no failed pool heartbeat observed during outage"
    start_server()
    wait_settled("server settled after restart")

    def recovered():
        status = obj("admin", "pool", "get", pool)
        (ARTIFACTS / "pool-after-restart.json").write_text(json.dumps(status, indent=2))
        return status.get("health") == "ready" and status.get("ready") and status.get("schedulable")
    try:
        eventually("existing pool ready after server restart", recovered, seconds=120)
        after = command(identity_args)
        (ARTIFACTS / "pool-agent-after-restart.log").write_text(after)
        assert before == after, "pool agent restarted during control-plane recovery"
        cli("send", box, 'printf \'restart-retained:%s\\n\' "$RESTART_SMOKE_TOKEN"')
        cli("send-key", box, "Enter")
        eventually("primary program retained after restart", lambda: "restart-retained:" + RUN in
                   "\n".join(obj("capture", box)["lines"]), seconds=15)
        cli("admin", "service", "ls", box)
        assert cli("exec", box, "--", "printf", "restart-recovered").strip() == "restart-recovered"
    finally:
        with sqlite3.connect(ARTIFACTS / "db.sqlite") as db:
            db.row_factory = sqlite3.Row
            row = db.execute("select health_check_started_at, status_reported_at, last_seen_at, "
                             "ready, schedulable, conditions, provision_progress from pools where id = ?",
                             (pool,)).fetchone()
            (ARTIFACTS / "pool-restart-persisted.json").write_text(json.dumps(dict(row), indent=2))
        journal = command(["docker", "exec", container, "journalctl", "-u", "discobox-pool-agent",
                           "--no-pager", "-n", "200"])
        (ARTIFACTS / "pool-restart-journal.log").write_text(journal)
    print("PASS server restart recovers pool, terminal and service access without agent restart", flush=True)


def smoke():
    primary = create_box("ux-primary", requester=True)
    restart_control_plane(primary)
    marker = "UX-" + RUN
    # Split the marker in the input: seeing the echoed command cannot pass capture.
    cli("send", primary, f"printf '%s%s\\n' UX- {RUN}")
    cli("send-key", primary, "Enter")
    answer = obj("wait", primary, "--until", "idle=1s", "--for", "15s")
    assert answer["reason"] != "timeout", answer
    screen = obj("capture", primary, "--scrollback", "100")
    (ARTIFACTS / "primary-screen.json").write_text(json.dumps(screen, indent=2))
    assert marker in "\n".join(screen.get("scrollback", []) + screen["lines"]), screen
    timed = json.loads(cli("wait", primary, "--until", "exit", "--for", "1s", expected=124))
    assert timed["reason"] == "timeout", timed
    print("PASS terminal send/send-key/wait/capture and timeout 124", flush=True)

    cli("exec", primary, "--", "sh", "-c", "printf '%s\\n' '{\"model\":\"reviewed\"}' > ~/.ux-smoke.json")
    cli("exec", primary, "--", "sh", "-c", "printf '%s' 'synthetic-not-a-real-secret' > ~/.ux-smoke-token")
    def captured():
        preview = obj("admin", "harness", "changes", primary)
        return preview if preview.get("candidateId") and preview.get("credentials") and preview.get("files") else None
    preview = eventually("settings and synthetic login captured", captured, seconds=30)
    (ARTIFACTS / "candidate.json").write_text(json.dumps(preview, indent=2))
    assert preview["state"] == "pending" and preview["candidateId"], preview
    assert any(f["path"] == ".ux-smoke.json" and
               json.loads(f["file"]["content"])["model"] == "reviewed" for f in preview["files"]), preview
    assert len(preview["credentials"]) == 1 and preview["credentials"][0]["envName"] == "UX_SMOKE_TOKEN", preview
    cli("admin", "harness", "save", primary, preview["candidateId"])
    saved = obj("admin", "harness", "get", "ux-smoke")
    assert any(f["path"] == ".ux-smoke.json" and
               json.loads(f["content"])["model"] == "reviewed" for f in saved["configuredFiles"]), saved
    assert saved["boundSecrets"] == 1, saved
    # Preserve an independent personal override through the default reset.
    cli("admin", "harness", "update", "ux-smoke", "--file", "personal.txt=personal-override")
    cli("admin", "harness", "reset", "ux-smoke")
    reset = obj("admin", "harness", "get", "ux-smoke")
    assert any(f["path"] == "personal.txt" and f["content"] == "personal-override" for f in reset["fileOverrides"]), reset
    assert not reset.get("configuredFiles") and reset["boundSecrets"] == 0, reset
    assert any(f["path"] == ".ux-smoke.json" and
               json.loads(f["content"])["model"] == "bundle" for f in reset["files"]), reset
    fresh = create_box("ux-reset", requester=True)
    assert cli("exec", fresh, "--", "sh", "-c", 'cat "$HOME/personal.txt"').strip() == "personal-override"
    assert json.loads(cli("exec", fresh, "--", "sh", "-c", 'cat "$HOME/.ux-smoke.json"'))["model"] == "bundle"
    print("PASS harness candidate review/save/reset preserves personal override", flush=True)

    requester = primary
    created = json.loads(cli("exec", fresh, "--", "discobox", "--output", "json",
                             "admin", "box", "create", "--name", "ux-adoptee", "--harness", "ux-smoke"))
    child = created["id"]
    BOXES.append(child)
    (ARTIFACTS / "boxes.json").write_text(json.dumps(BOXES))
    wait_box(child)
    cli("exec", fresh, "--", "discobox", "release", child)
    before = obj("admin", "box", "get", child)
    creator = before.get("createdBySandboxId")
    assert creator == fresh and not before.get("parentSandboxId"), before
    # These calls execute the shipped CLI inside a real box. The pool authenticates
    # its sandbox identity; the host CLI/API is the local person principal.
    for decision in ("deny", "approve"):
        with (ARTIFACTS / (decision + "-request.log")).open("w") as log:
            process = subprocess.Popen(CLI + ["exec", requester, "--", "discobox", "adopt", child,
                                              "--reason", "synthetic UX smoke " + decision],
                                       cwd=ROOT, env=ENV, stdin=subprocess.DEVNULL,
                                       stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            try:
                def pending():
                    inbox = api("approval-requests?status=pending")["approvalRequests"]
                    return next((i["adoption"] for i in inbox if i["kind"] == "adoption"
                                 and i["adoption"]["requesterSandboxId"] == requester), None)
                request = eventually("adoption in person inbox", pending, seconds=30)
                assert request["childSandboxId"] == child and request["status"] == "pending", request
                api("adoption-requests/" + request["id"] + "/" + decision, "POST")
                code = process.wait(timeout=30)
                assert (code == 0) == (decision == "approve"), code
                state = api("adoption-requests/" + request["id"])
                assert state["status"] == ("approved" if decision == "approve" else "denied"), state
                assert not pending(), "answered request remains in inbox"
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
        after = obj("admin", "box", "get", child)
        assert after.get("createdBySandboxId") == creator, after
        assert after.get("parentSandboxId") == (requester if decision == "approve" else None), after
    cli("exec", requester, "--", "discobox", "release", child)
    after = obj("admin", "box", "get", child)
    assert not after.get("parentSandboxId") and after.get("createdBySandboxId") == creator, after
    print("PASS sandbox adoption denial/approval inbox transitions and release", flush=True)


def main():
    global SERVER, BUILDER_CREATED, BUILDER, CLI, URL
    for tool in ("docker", "go", "jq"):
        if not shutil.which(tool):
            raise RuntimeError(tool + " is required; missing prerequisites are missing coverage")
    store = json.loads(command(["docker", "info", "--format", "{{json .DriverStatus}}"], limit=20))
    if ["driver-type", "io.containerd.snapshotter.v1"] in store:
        # Match the image watcher's localBundleBuilder: the host runtime carries
        # a discobox's proxy and CA into build steps, and can export OCI here.
        BUILDER = command(["docker", "context", "show"]).strip()
    else:
        command(["docker", "buildx", "create", "--name", BUILDER, "--driver", "docker-container"])
        BUILDER_CREATED = True
    # Bootstrap once before concurrent clients connect. A running container is
    # not necessarily ready to serve its BuildKit socket yet.
    command(["docker", "buildx", "inspect", "--bootstrap", BUILDER], limit=180)
    bundles = build_stack()
    image_id = command(["docker", "image", "inspect", BASE, "--format", "{{.Id}}"]).strip()
    manifest = ARTIFACTS / "manifest.json"
    manifest.write_text(json.dumps({"version": 1, "images": [{"reference": BASE, "id": image_id}], "bundles": bundles}))
    with socket.socket() as sock:
        sock.bind(("0.0.0.0", 0))
        port = sock.getsockname()[1]
    URL = f"http://127.0.0.1:{port}"
    ENV.update({"PORT": str(port), "DISCOBOX_SERVER_LISTEN": f"unix://{ARTIFACTS}/server.sock,http://127.0.0.1:{port}",
                "DATABASE_DSN": str(ARTIFACTS / "db.sqlite"),
                "DISCOBOX_DEV_DOCKER_IMAGE_SYNC": "true", "DISCOBOX_DEV_DOCKER_IMAGE_MANIFEST": str(manifest),
                "DISCOBOX_DOCKER_SANDBOX_BASE_IMAGE": BASE,
                "DISCOBOX_POOL_AGENT_BUNDLE": bundles[0]["reference"],
                "DISCOBOX_AGENT_BUNDLE": bundles[1]["reference"],
                "DISCOBOX_PLATFORM_BUNDLE": bundles[2]["reference"]})
    # Boot with synthetic built-ins too: a fresh server requires a runnable
    # harness before opening its listeners, and must not pull released agents.
    for slug in ("CLAUDE_CODE", "CODEX", "OPENCODE", "COPILOT", "OMP", "PI", "SHELL"):
        ENV[f"DISCOBOX_HARNESS_{slug}_BUNDLE"] = bundles[3]["reference"]
    CLI = [str(ARTIFACTS / "discobox"), "--server", URL, "--project", "default",
           "--auto-start-server=false", "--output", "json"]
    start_server()
    pools = obj("admin", "pool", "ls")["pools"]
    POOLS.extend(p["id"] for p in pools)
    assert POOLS, "isolated server seeded no pool"
    (ARTIFACTS / "pools.json").write_text(json.dumps(POOLS))
    eventually("pool ready", lambda: any(p.get("ready") and p.get("schedulable")
                                         for p in obj("admin", "pool", "ls")["pools"]), seconds=180)
    cli("admin", "harness", "create", "--bundle", bundles[3]["reference"], "--slug", "ux-smoke")
    started = time.monotonic()
    smoke()
    print(f"TIMING flows {time.monotonic()-started:.1f}s", flush=True)


def purge_box(box):
    try:
        cli("admin", "box", "purge", box, input="", limit=45)
        return
    except CommandFailure as exc:
        # Only this answer promises a durable background delete. Do not resend
        # purge: observe its outcome before removing the pool it needs to finish.
        pending = ("409 Conflict: sandbox purge did not complete; "
                   "it will be retried in the background")
        if exc.result.returncode != 1 or pending not in exc.result.stderr:
            raise

    last = None
    def removed():
        nonlocal last
        try:
            last = api("sandboxes/" + box)
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                return True
            raise
        # Retain the state and errorMessage missing from the original flake's
        # artifacts, including when background convergence never completes.
        (ARTIFACTS / (box + "-purge.json")).write_text(json.dumps(last, indent=2))
        return False

    try:
        eventually("background purge of " + box, removed, seconds=60)
    except Exception as exc:
        raise RuntimeError(f"background purge of {box} failed; last sandbox: {last}") from exc


def cleanup(failed):
    started = time.monotonic()
    errors = []
    diagnostic_errors = []
    # Discover only IDs in this run's private database, including bootstrap failures.
    db = ARTIFACTS / "db.sqlite"
    if db.exists():
        import sqlite3
        try:
            with sqlite3.connect(db) as con:
                POOLS[:] = [row[0] for row in con.execute("select id from pools")]
        except sqlite3.Error as exc:
            errors.append(str(exc))
    for box in BOXES:
        try:
            (ARTIFACTS / (box + "-screen.json")).write_text(cli("capture", box, "--scrollback", "100"))
        except Exception as exc:
            diagnostic_errors.append(f"screen {box}: {exc}")
    for pool in POOLS:
        try:
            ids = command(["docker", "ps", "-aq", "--filter", "label=discobox.pool_id=" + pool]).split()
        except Exception as exc:
            diagnostic_errors.append(f"containers in pool {pool}: {exc}")
            continue
        for container in ids:
            try:
                logs = command(["docker", "logs", "--tail", "300", container])
                (ARTIFACTS / (container + ".log")).write_text(logs)
            except Exception as exc:
                diagnostic_errors.append(f"logs {container}: {exc}")
            try:
                running = command(["docker", "inspect", container, "--format", "{{.State.Running}}"]).strip()
                if running == "true":
                    # systemd images keep the agent's output in the journal.
                    journal = command(["docker", "exec", container, "journalctl", "--no-pager", "-n", "300"])
                    (ARTIFACTS / (container + "-journal.log")).write_text(journal)
            except Exception as exc:
                diagnostic_errors.append(f"journal {container}: {exc}")
    for box in reversed(BOXES):
        try:
            purge_box(box)
        except Exception as exc:
            errors.append(str(exc))
    if CLI and POOLS:
        try:
            cli("admin", "pool", "unset-default")
        except Exception as exc:
            errors.append(str(exc))
    for pool in POOLS:
        try:
            cli("admin", "pool", "rm", pool, limit=30)
            eventually("pool removed", lambda: all(p["id"] != pool for p in
                       (obj("admin", "pool", "ls").get("pools") or [])), seconds=30)
        except Exception as exc:
            errors.append(str(exc))
    if SERVER and SERVER.poll() is None:
        os.killpg(SERVER.pid, signal.SIGTERM)
        try:
            SERVER.wait(timeout=15)
        except subprocess.TimeoutExpired:
            os.killpg(SERVER.pid, signal.SIGKILL)
            SERVER.wait()
    # Backstop exclusively scoped to pool IDs from our database. No image sweep.
    for pool in POOLS:
        try:
            ids = command(["docker", "ps", "-aq", "--filter", "label=discobox.pool_id=" + pool]).split()
            if ids:
                command(["docker", "rm", "-f"] + ids)
            networks = command(["docker", "network", "ls", "-q", "--filter", "name=^discobox-sbnet-" + pool + "$"]).split()
            if networks:
                command(["docker", "network", "rm"] + networks)
        except Exception as exc:
            errors.append(str(exc))
    if BUILDER_CREATED:
        if failed:
            # Preserve daemon state before removing our builder. In particular,
            # inspect retains OOMKilled/ExitCode and logs retain startup errors.
            container = "buildx_buildkit_" + BUILDER + "0"
            for args in (["docker", "inspect", container],
                         ["docker", "logs", "--tail", "300", container]):
                try:
                    command(args, limit=20)
                except Exception as exc:
                    diagnostic_errors.append(f"builder diagnostics {args}: {exc}")
        try:
            command(["docker", "buildx", "rm", BUILDER], limit=60)
        except Exception as exc:
            errors.append(str(exc))
    try:
        if command(["docker", "image", "ls", "-q", BASE]).strip():
            command(["docker", "image", "rm", BASE])
    except Exception as exc:
        errors.append(str(exc))
    # OCI archives and compiled binaries are build inputs, not diagnostics.
    for name in ("pool.tar", "agent.tar", "platform.tar", "harness.tar", "discobox", "discobox-server"):
        (ARTIFACTS / name).unlink(missing_ok=True)
    if diagnostic_errors:
        (ARTIFACTS / "diagnostic-errors.log").write_text("\n".join(diagnostic_errors))
        print("WARNING: diagnostic collection incomplete: " + "; ".join(diagnostic_errors), flush=True)
    if errors:
        (ARTIFACTS / "cleanup-errors.log").write_text("\n".join(errors))
        if not failed:
            raise RuntimeError("cleanup failed: " + "; ".join(errors))
    print(f"TIMING cleanup {time.monotonic()-started:.1f}s", flush=True)
    print("Artifacts:", ARTIFACTS, flush=True)


def interrupted(signum, _frame):
    raise RuntimeError(f"smoke interrupted by signal {signum}")


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    failed = True
    try:
        main()
        failed = False
    finally:
        cleanup(failed)
