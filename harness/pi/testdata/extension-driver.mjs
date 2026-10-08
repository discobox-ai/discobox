// Runs the hook extension against a synthetic pi and prints, as a JSON array,
// the events it published in order, each with the payload it sent. Run by
// driver_test.go; it lives here so the plugin's behaviour is executed rather
// than read.
import { mkdtempSync, readFileSync, writeFileSync, chmodSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const dir = mkdtempSync(join(tmpdir(), "pi-hook-"));
const log = join(dir, "published.jsonl");
// A publisher that records what it was told and what it was given.
writeFileSync(
  join(dir, "discobox-hook-publish"),
  `#!/bin/sh\nprovider=$2; event=$4; payload=$(cat)\nprintf '{"provider":"%s","event":"%s","payload":%s}\\n' "$provider" "$event" "$payload" >>"${log}"\n`,
);
chmodSync(join(dir, "discobox-hook-publish"), 0o755);
process.env.PATH = `${dir}:${process.env.PATH}`;
process.env.DISCOBOX_HOOK_SOCKET = "/run/discobox/hook.sock";

const handlers = new Map();
const pi = {
  on(event, handler) {
    handlers.set(event, handler);
    return () => handlers.delete(event);
  },
};
const { default: register } = await import(pathToFileURL(new URL("../hook-extension.js", import.meta.url).pathname).href);
register(pi);

const fire = async (event) => {
  const handler = handlers.get(event.type);
  if (handler) await handler(event, {});
};

await fire({ type: "session_start" });
await fire({ type: "before_agent_start", prompt: "fix the tests", images: [{ data: "..." }], systemPrompt: "you are pi" });
await fire({ type: "agent_start" });
await fire({ type: "turn_start", turnIndex: 0 });
await fire({ type: "tool_call", toolName: "bash", toolCallId: "call_1", input: { command: "ls -la" } });
await fire({ type: "tool_result", toolName: "bash", toolCallId: "call_1", input: { command: "ls -la" }, content: [{ type: "text", text: "a huge listing" }], isError: false });
await fire({ type: "message_update", assistantMessageEvent: { type: "text_delta", delta: "x" } });
await fire({ type: "turn_end", turnIndex: 0, message: { role: "assistant" }, toolResults: [] });
await fire({ type: "agent_end", messages: [{ role: "assistant" }], willRetry: false });
await fire({ type: "agent_settled" });
await fire({ type: "model_select", model: { provider: "anthropic", id: "claude-sonnet-5", name: "Claude" }, previousModel: undefined, source: "user" });
await fire({ type: "session_shutdown" });

const published = readFileSync(log, "utf8")
  .split("\n")
  .filter(Boolean)
  .map((line) => JSON.parse(line));
process.stdout.write(JSON.stringify(published));
