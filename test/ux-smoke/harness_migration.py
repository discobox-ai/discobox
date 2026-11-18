#!/usr/bin/env python3
"""Upgrade a v0.19.5 database and its Docker homes through the public CLI."""
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
import shlex
import signal
import urllib.request

import run as stack

RELEASE = "v0.19.5"
RELEASE_BASE = f"ghcr.io/discobox-ai/discobox-sandbox-base:{RELEASE}"
DIGESTS = {
    "discobox-linux-amd64": "5662fb2a18aeb5611579431353ed0d7a0b51327c03f076ec4353d4983dfbed1b",
    "discobox-server-linux-amd64": "1ed7d18083196f95ec5c91d3f665101481845c44f826fe538424595ee50457ef",
}
FIXTURES = json.loads((stack.ROOT / "test/harness-migration/homes.json").read_text())
LITERAL = "${fixture.unknown}"
CHILD = "fixture-child"
SENTINEL_CONTEXT = '"$HOME/.config/discobox/harness-sentinels.json"'
READ_SENTINEL = "jq -er --arg name \"$1\" '.sentinels[$name] | strings | select(length > 0)' " + SENTINEL_CONTEXT + " 2>/dev/null"


def release_binaries():
    paths = []
    for name, expected in DIGESTS.items():
        path = stack.ARTIFACTS / name
        url = f"https://github.com/discobox-ai/discobox/releases/download/{RELEASE}/{name}"
        with urllib.request.urlopen(url, timeout=120) as response, path.open("wb") as output:
            digest = hashlib.sha256()
            while chunk := response.read(1024 * 1024):
                digest.update(chunk)
                output.write(chunk)
        assert digest.hexdigest() == expected, f"unexpected digest for {name}"
        path.chmod(0o700)
        paths.append(path)
    return paths


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
        result = stack.obj("admin", "harness", "changes", box)
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
            if "409 Conflict: harness capture request was refused" not in exc.result.stderr:
                raise
            # The agent re-observed a concurrent home write between preview
            # and save. Review the latest version of our controlled change.
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
    if "export" not in fixture:
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
for (const pid of fs.readdirSync("/proc")) {
  if (!/^\d+$/.test(pid) || Number(pid) === process.pid) continue;
  try {
    const env = fs.readFileSync(`/proc/${pid}/environ`, "utf8").split("\0");
    if (!env.includes("HOME=" + process.env.HOME)) continue;
    const args = fs.readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0");
    if (args.some(arg => path.basename(arg) === name)) process.kill(Number(pid));
  } catch (error) {
    if (!["EACCES", "ENOENT", "EPERM", "ESRCH"].includes(error.code)) throw error;
  }
}
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


def create_fixture(release_cli, release_server):
    select_cli(release_cli)
    # Keep a cold image download outside the released pool bootstrap deadline.
    stack.command(["docker", "pull", RELEASE_BASE], limit=600)
    stack.start_server(release_server, seconds=180)
    pools = stack.obj("admin", "pool", "ls")["pools"]
    stack.POOLS.extend(p["id"] for p in pools)
    stack.eventually("release pool ready", lambda: any(
        p.get("ready") and p.get("schedulable")
        for p in stack.obj("admin", "pool", "ls")["pools"]), seconds=360)
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


def main():
    stack.configure_server()
    release_cli, release_server = release_binaries()
    harnesses = [slug for slug in FIXTURES if slug != CHILD]
    stack.prepare_builder()
    bundles = stack.build_stack(harnesses)
    boxes, baseline = create_fixture(release_cli, release_server)
    stop_server()
    stack.configure_current_bundles(bundles)
    for slug, bundle in zip(harnesses, bundles[3:], strict=True):
        stack.ENV["DISCOBOX_HARNESS_" + slug.upper().replace("-", "_") + "_BUNDLE"] = bundle["reference"]
    select_cli(stack.ARTIFACTS / "discobox")
    stack.start_server(seconds=180)
    stack.wait_settled("upgraded server and pool")
    upgrade_fixture(boxes, baseline)
    print("PASS v0.19.5 built-in harness migration", flush=True)


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, stack.interrupted)
    failed = True
    try:
        main()
        failed = False
    finally:
        stack.cleanup(failed)
        for name in DIGESTS:
            (stack.ARTIFACTS / name).unlink(missing_ok=True)
