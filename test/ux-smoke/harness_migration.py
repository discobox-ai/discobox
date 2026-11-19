#!/usr/bin/env python3
"""Upgrade a v0.19.5 database and its Docker homes through the public CLI."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
import platform
import re
import shlex
import signal
import shutil

import run as stack

RELEASE = "v0.19.5"
RELEASE_ASSETS = "https://assets.discobox.ai/discobox"
BASELINE_DIGESTS = {
    "discobox": "5662fb2a18aeb5611579431353ed0d7a0b51327c03f076ec4353d4983dfbed1b",
    "discobox-server": "1ed7d18083196f95ec5c91d3f665101481845c44f826fe538424595ee50457ef",
}
FIXTURES = json.loads((stack.ROOT / "test/harness-migration/homes.json").read_text())
LITERAL = "${fixture.unknown}"
CHILD = "fixture-child"
SENTINEL_CONTEXT = '"$HOME/.config/discobox/harness-sentinels.json"'
READ_SENTINEL = "jq -er --arg name \"$1\" '.sentinels[$name] | strings | select(length > 0)' " + SENTINEL_CONTEXT + " 2>/dev/null"


def download_release(version):
    directory = stack.ARTIFACTS / version
    directory.mkdir()
    root = f"{RELEASE_ASSETS}/{version}"
    manifest_path = directory / "manifest.json"
    stack.command(["curl", "-fsSL", "-o", manifest_path, f"{root}/discobox-manifest-linux-amd64.json"])
    manifest = json.loads(manifest_path.read_text())
    if manifest["version"] != version:
        raise ValueError(f"expected manifest for {version}, got {manifest['version']}")
    for role, name in (("clients", "discobox"), ("servers", "discobox-server")):
        [binary] = [binary for binary in manifest[role]
                    if (binary["os"], binary["arch"], binary["command"]) == ("linux", "amd64", name)]
        [asset] = [asset for asset in binary["assets"] if asset["name"] == name]
        expected = asset["sha256"]
        if version == RELEASE and expected != BASELINE_DIGESTS[name]:
            raise ValueError(f"baseline manifest digest changed for {name}")
        path = directory / name
        url = next((url for url in asset["urls"] if url.startswith(root + "/")), None)
        if url is None:
            raise ValueError(f"missing release asset URL for {version} {name}")
        stack.command(["curl", "-fsSL", "-o", path, url], limit=600)
        with path.open("rb") as binary_file:
            digest = hashlib.file_digest(binary_file, "sha256").hexdigest()
        if digest != expected:
            raise ValueError(f"unexpected digest for {version} {name}")
        path.chmod(0o700)
    return directory, manifest["images"]["sandboxBase"]


def start_release(directory, base):
    select_cli(directory / "discobox")
    stack.ENV["DISCOBOX_RELEASE_MANIFEST"] = str(directory / "manifest.json")
    # Keep a cold image download outside pool bootstrap's deadline.
    stack.command(["docker", "pull", base], limit=600)
    stack.start_server(directory / "discobox-server", seconds=180)


def select_cli(binary):
    stack.CLI = [str(binary), "--server", stack.URL, "--project", "default",
                 "--auto-start-server=false", "--output", "json"]


def config(slug):
    return stack.obj("admin", "harness", "get", slug)


def run_in_box(box, *args, input=None):
    return stack.cli("exec", box, "--", *args, input=input)


def write_home(box, path, content):
    run_in_box(box, "sh", "-ec",
               'mkdir -p "$HOME/$(dirname "$1")"; cat > "$HOME/$1"',
               "fixture", path, input=content)


def settings(path, value):
    if path.endswith((".yml", ".toml")):
        separator = ": " if path.endswith(".yml") else " = "
        return f"migrationPreference{separator}{json.dumps(value)}\n"
    return json.dumps({"migrationPreference": value}) + "\n"


def bindings(slug):
    response = stack.obj("admin", "harness", "secret", "ls", slug)
    return sorted((b["id"], b["targetId"], b["secretId"], b["envVar"])
                  for b in response.get("secretBindings") or [])


def state(slug):
    harness = config(slug)
    result = {key: harness.get(key) for key in
              ("id", "extends", "configuredFiles", "fileOverrides", "boundSecrets")}
    result["bindings"] = bindings(slug)
    return result


def pending(box, path, credential_count):
    def candidate():
        try:
            result = stack.obj("admin", "harness", "changes", box)
        except stack.CommandFailure as exc:
            if "503 harness capture request was refused" not in exc.result.stderr:
                raise
            return None
        if (result.get("candidateId")
                and len(result.get("credentials") or []) == credential_count
                and any(f["path"] == path for f in result.get("files") or [])):
            return result
        return None
    return stack.eventually("capture " + path, candidate, seconds=45)


def save(box, candidate):
    result = stack.obj("admin", "harness", "save", box, candidate["candidateId"])
    assert result["state"] in ("saved", "saved-handoff-pending"), result
    return result["state"]


def save_change(box, path, credential_count, expected):
    def attempt():
        candidate = pending(box, path, credential_count)
        assert any(expected in f["file"]["content"]
                   for f in candidate["files"] if f["path"] == path), candidate
        try:
            return candidate, save(box, candidate)
        except stack.CommandFailure as exc:
            retryable = (
                "409 Conflict: harness capture request was refused",
                "503 Service Unavailable: harness capture request was refused",
            )
            if not any(message in exc.result.stderr for message in retryable):
                raise
            # The native store may still be settling after restart. Review
            # the controlled change again before trying a fresh candidate.
            return None
    return stack.eventually("save " + path, attempt, seconds=60)


def wait_home(box):
    def ready():
        runtime = stack.obj("admin", "box", "get", box)["runtime"]
        return runtime.get("displayState") == "running" and runtime.get("agentStatus", {}).get(
            "provision", {}).get("ready")
    stack.eventually("provisioned home " + box, ready, seconds=600)


def make_home(slug, fixture, box):
    wait_home(box)
    write_home(box, "local-personal.txt", LITERAL)
    if not fixture:
        return
    stop_fixture_app(box, slug)
    path = fixture["settings"]
    write_home(box, path, settings(path, LITERAL))
    document = json.dumps(fixture["document"])
    if "login" in fixture:
        write_home(box, fixture["login"], document)
    elif slug == "omp":
        run_in_box(box, "sh", "-ec", "f=$(mktemp); trap 'rm \"$f\"' EXIT; cat > \"$f\"; omp auth import \"$f\"",
                   input=document)
    else:
        run_in_box(box, *fixture["import"], input=document)
    if slug == "omp":
        account = fixture["document"]["credentials"][0]["credential"]["accountId"]
        run_in_box(box, "sh", "-ec",
                   "omp auth export | jq -e --arg account \"$1\" "
                   "'.credentials | any(.credential.accountId == $account)' >/dev/null",
                   "fixture", account)
    candidate, state = save_change(box, path, credential_count=1, expected=LITERAL)
    assert_saved(slug, path, LITERAL)
    if state == "saved-handoff-pending" and "export" in fixture:
        # Open native stores defer handoff until the coding process exits.
        stop_fixture_app(box, slug)
        stack.eventually("replayed native handoff " + slug,
                         lambda: save(box, candidate) == "saved", seconds=120)


def stop_fixture_app(box, slug):
    # End only the fixture app for this home, leaving its login shell running.
    names = {"claude-code": "claude", CHILD: "codex"}
    run_in_box(box, "node", "-e", r"""
const fs = require("node:fs");
const path = require("node:path");
const name = process.argv[1];
const stopped = [];
for (const pid of fs.readdirSync("/proc")) {
  if (!/^\d+$/.test(pid) || Number(pid) === process.pid) continue;
  try {
    const env = fs.readFileSync(`/proc/${pid}/environ`, "utf8").split("\0");
    if (!env.includes("HOME=" + process.env.HOME)) continue;
    const args = fs.readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0");
    // The executable or script names the app. Capture workers pass the
    // harness name as a later argument, and auth subcommands are short-lived.
    if (args.slice(0, 2).some(arg => path.basename(arg) === name) && !args.includes("auth")) {
      process.kill(Number(pid));
      stopped.push(pid);
    }
  } catch (error) {
    if (!["EACCES", "ENOENT", "EPERM", "ESRCH"].includes(error.code)) throw error;
  }
}
const deadline = Date.now() + 10000;
function running(pid) {
  try {
    return fs.readFileSync(`/proc/${pid}/cmdline`).length > 0;
  } catch (error) {
    if (["ENOENT", "ESRCH"].includes(error.code)) return false;
    throw error;
  }
}
function waitForExit() {
  if (!stopped.some(running)) return;
  if (Date.now() >= deadline) {
    process.exitCode = 1;
    return;
  }
  setTimeout(waitForExit, 50);
}
waitForExit();
""", names.get(slug, slug))


def assert_saved(slug, path, expected):
    files = config(slug).get("configuredFiles") or []
    assert any(f["path"] == path and expected in f["content"] for f in files), (slug, path, files)


def effective_home(slug, box, fixture, old):
    personal = run_in_box(box, "sh", "-ec", 'cat "$HOME/personal.txt" "$HOME/local-personal.txt"')
    assert personal == LITERAL * 2, personal
    if fixture:
        path = fixture["settings"]
        text = run_in_box(box, "sh", "-ec", 'cat "$HOME/$1"', "fixture", path)
        assert "migrationPreference" in text and LITERAL in text, (path, text)
        env = old["bindings"][0][3]
        # Keep raw home data and sentinel values off the host and out of logs.
        if slug == "copilot":
            run_in_box(box, "sh", "-ec", READ_SENTINEL + " >/dev/null || exit 1", "fixture", env)
        else:
            source = shlex.join(fixture["export"]) if "export" in fixture else 'cat "$HOME/$2"'
            run_in_box(box, "sh", "-ec",
                       'sentinel=$(' + READ_SENTINEL + ') || exit 1; '
                       + source + ' | grep -Fq -- "$sentinel"',
                       "fixture", env, fixture.get("login", ""))


def home_ready(slug, box, expected):
    try:
        effective_home(slug, box, FIXTURES[slug], expected)
    except stack.CommandFailure as exc:
        if exc.result.returncode == 1:
            return False
        raise
    return True


def stop_server():
    os.killpg(stack.SERVER.pid, signal.SIGTERM)
    assert stack.SERVER.wait(timeout=30) == 0


def create_fixture():
    pools = stack.obj("admin", "pool", "ls")["pools"]
    stack.POOLS.extend(p["id"] for p in pools)
    pool_id = stack.obj("admin", "project", "get", "default")["defaultPoolId"]
    [pool] = [pool for pool in pools if pool["id"] == pool_id]
    provider = stack.obj("admin", "provider", "get", pool["providerInstanceId"])
    if provider["type"] != "docker":
        raise RuntimeError("harness migration requires a host where the baseline defaults to Docker")

    def pool_ready():
        pool = stack.obj("admin", "pool", "get", pool_id)
        return pool.get("ready") and pool.get("schedulable")

    stack.eventually("release Docker pool ready", pool_ready, seconds=360)
    stack.cli("admin", "project", "update", "default", "--sandbox-upgrade-policy", "manual")
    stack.cli("admin", "harness", "create", "--name", "Migration child", "--slug", CHILD, "--extends", "codex")
    stack.cli("admin", "harness", "set-default", CHILD)
    boxes = {}
    for slug in FIXTURES:
        stack.cli("admin", "harness", "update", slug, "--file", "personal.txt=" + LITERAL)
        args = [] if slug == CHILD else ["--harness", slug]
        box = stack.obj("admin", "box", "create", "--name", "migration-" + slug, "--no-project-skills", *args)
        boxes[slug] = box["id"]
        stack.BOXES.append(box["id"])
    with ThreadPoolExecutor(max_workers=3) as workers:
        list(workers.map(lambda slug: make_home(slug, FIXTURES[slug], boxes[slug]), FIXTURES))
    baseline = {slug: state(slug) for slug in FIXTURES}
    for slug, old in baseline.items():
        assert len(old["bindings"]) == bool(FIXTURES[slug]), (slug, old)
        assert all(b[1] == old["id"] for b in old["bindings"]), (slug, old)
        stack.eventually("release home handed off " + slug,
                         lambda: home_ready(slug, boxes[slug], old), seconds=120)
    assert baseline[CHILD]["extends"] == baseline["codex"]["id"]
    assert baseline[CHILD]["bindings"][0][2] != baseline["codex"]["bindings"][0][2]
    assert stack.obj("admin", "project", "get", "default")["defaultHarnessConfigId"] == baseline[CHILD]["id"]
    stage_prior_alias(boxes["codex"], baseline["codex"]["bindings"][0][3])
    # Leave a reviewed-but-unsaved edit in the child on the released agent.
    path = FIXTURES[CHILD]["settings"]
    write_home(boxes[CHILD], path, settings(path, LITERAL + " pending across upgrade"))
    pending(boxes[CHILD], path, credential_count=0)
    return boxes, baseline


def stage_prior_alias(box, env):
    # A box-specific binding with a different format makes the pool remint
    # this box's sentinel while the saved harness account stays unchanged.
    run_in_box(box, "sh", "-ec",
               'cp "$HOME/.codex/auth.json" "$HOME/.codex/migration-prior-auth.json"')
    secret = stack.obj("secret", "create", "--name", "migration-remint",
                       "--token", "fixture-remint-key", "--format", "sk-fixture-{hex:24}",
                       "--route", "api.openai.com")
    stack.cli("secret", "bind", secret["id"], box, "--as", env)
    stack.eventually("reminted Codex assignment", lambda: run_in_box(
        box, "sh", "-ec", 'case "$(' + READ_SENTINEL + ')" in sk-fixture-*) printf ready;; esac',
        "fixture", env) == "ready", seconds=90)
    stack.eventually("prior alias recorded", lambda: has_prior_alias(box, env), seconds=90)


def has_prior_alias(box, env):
    # Compare inside the box without logging either the old or current value.
    return run_in_box(
        box, "sh", "-ec",
        'old=$(jq -r .OPENAI_API_KEY "$HOME/.codex/migration-prior-auth.json"); '
        'if test -n "$old" && jq -e --arg env "$1" --arg old "$old" '
        '\'(.knownSentinels[$env] // []) | index($old) != null\' '
        '"$HOME/.config/discobox/harness-sentinels.json" >/dev/null 2>&1; then printf ready; fi',
        "fixture", env) == "ready"


def upgrade_fixture(boxes, baseline):
    for slug, box in boxes.items():
        if slug != CHILD:
            stack.cli("admin", "harness", "refresh", slug)
        assert state(slug) == baseline[slug], slug
        target = config(slug)["bundleDigest"]
        current = stack.obj("admin", "box", "get", box)
        if current["config"]["bundleDigest"] != target or current["runtime"]["upgrade"]["available"]:
            stack.cli("admin", "box", "upgrade", box)
        stack.eventually("new bundle for " + slug, lambda:
                         stack.obj("admin", "box", "get", box)["config"]["bundleDigest"] == target,
                         seconds=300)
        wait_home(box)
        stack.eventually("upgraded login and home " + slug,
                         lambda: home_ready(slug, box, baseline[slug]), seconds=120)
        stack.cli("admin", "box", "stop", box)
        stack.eventually("stopped " + slug, lambda: stack.obj("admin", "box", "get", box)["runtime"]
                         .get("displayState") == "stopped", seconds=90)
        stack.cli("admin", "box", "start", box)
        wait_home(box)
        stack.eventually("restarted login and home " + slug,
                         lambda: home_ready(slug, box, baseline[slug]), seconds=120)
        # Saving a settings edit with a delivered login present must keep its
        # binding. A sentinel re-captured as a new credential would replace it.
        fixture = FIXTURES[slug]
        if fixture:
            if "export" not in fixture:
                stop_fixture_app(box, slug)
            path = fixture["settings"]
            expected = LITERAL + (" pending across upgrade" if slug == CHILD else " after restart")
            if slug == "codex":
                assert has_prior_alias(box, baseline[slug]["bindings"][0][3]), "lost prior Codex sentinel"
                run_in_box(box, "sh", "-ec",
                           'mv "$HOME/.codex/migration-prior-auth.json" "$HOME/.codex/auth.json"')
            if slug != CHILD:
                write_home(box, path, settings(path, expected))
            candidate, _ = save_change(box, path, credential_count=0, expected=expected)
            if slug == "codex":
                assert not candidate.get("diagnostics"), candidate
            assert_saved(slug, path, expected)
            assert bindings(slug) == baseline[slug]["bindings"], slug
        assert config(slug)["id"] == baseline[slug]["id"]
    assert stack.obj("admin", "project", "get", "default")["defaultHarnessConfigId"] == baseline[CHILD]["id"]
    # Reset the child first so its independent account is observed before the parent resets.
    for slug in (CHILD, *(s for s in boxes if s != CHILD)):
        stack.cli("admin", "harness", "reset", slug)
        reset = config(slug)
        assert not reset.get("configuredFiles") and not bindings(slug), slug
        assert reset["fileOverrides"] == baseline[slug]["fileOverrides"], slug


def main(candidate):
    if platform.system() != "Linux" or platform.machine() != "x86_64":
        raise RuntimeError("harness migration requires a Linux amd64 Docker host")
    stack.configure_server()
    baseline_release, baseline_base = download_release(RELEASE)
    candidate_release, candidate_base = download_release(candidate)
    start_release(baseline_release, baseline_base)
    boxes, baseline = create_fixture()
    stop_server()
    start_release(candidate_release, candidate_base)
    stack.wait_settled("upgraded server and pool")
    upgrade_fixture(boxes, baseline)
    print(f"PASS {RELEASE} to {candidate} built-in harness migration", flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("candidate", help="Published Discobox candidate tag, such as v0.20.0-rc.2")
    candidate = parser.parse_args().candidate
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*)?", candidate) or candidate == RELEASE:
        parser.error("candidate must be a published release tag different from the fixture release")
    signal.signal(signal.SIGTERM, stack.interrupted)
    failed = True
    try:
        main(candidate)
        failed = False
    finally:
        try:
            stack.cleanup(failed)
        finally:
            for version in (RELEASE, candidate):
                shutil.rmtree(stack.ARTIFACTS / version, ignore_errors=True)
