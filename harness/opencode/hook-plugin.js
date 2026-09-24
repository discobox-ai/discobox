// Publishes opencode's lifecycle events to the Discobox sandbox agent.
//
// The other two harness images declare their hooks as commands in a managed
// settings file and the CLI runs them. opencode has no command hook: its
// lifecycle reaches JavaScript and nothing else, so this plugin is the
// equivalent unit — image-owned, version-coupled to the CLI it ships beside,
// and reached from opencode's managed config layer (`/etc/opencode`) rather
// than from the user's own config, which the configure flow replaces.
//
// It publishes through `discobox-hook-publish`, the same generic publisher the
// other two images' hooks invoke, so the wire protocol has one implementation
// and this file never learns it. The publisher takes its payload on stdin and
// reads the terminal ID and socket path from the environment opencode already
// runs in.

const PROVIDER = "opencode";

// Bus events worth recording, named exactly as opencode emits them.
//
// An allowlist, not a denylist. opencode's bus carries per-token streaming
// events — `message.part.updated` fires for every delta of every message — and
// publishing one spawns a process and writes a row. A denylist would publish
// each new event opencode ships by default, and a stream-rate one would flood
// a sandbox's hook table until somebody noticed; an allowlist fails the other
// way, by missing a new event until it is added here.
//
// Tool calls are deliberately absent: opencode delivers those to the
// `tool.execute.before` / `.after` hooks rather than to the bus, and they are
// published from there.
const PUBLISHED_EVENTS = new Set([
  "session.created",
  "session.idle",
  "session.error",
  "session.compacted",
  "session.deleted",
  "permission.asked",
  "permission.replied",
  "file.edited",
  "command.executed",
  "todo.updated",
  "server.connected",
  "installation.updated",
]);

// The four session lifecycle events opencode publishes per session rather than
// per turn, and which therefore mean the root session's here. opencode's task
// tool runs a sub-session, and each one publishes its own `session.created`,
// `session.idle`, `session.error` and `session.compacted` — opencode's own
// `run` command filters by root session id for exactly this reason. A wait
// matches by name and the name carries no session, so publishing a child's
// idle would end a `--hook Stop` at the first subagent to finish, which is the
// false turn-end ADR 0137's matcher exists to avoid.
//
// `session.deleted` is deliberately NOT here. It has no canonical name, so it
// cannot be mistaken for the root's turn ending, and a child's deletion is a
// fact worth recording under opencode's own name — which is what ADR 0146 §3
// keeps un-canonical events around for. It is also what bounds the set below.
const ROOT_ONLY_EVENTS = new Set([
  "session.created",
  "session.idle",
  "session.error",
  "session.compacted",
]);

export default async ({ $ }) => {
  // Sessions known to have a parent, learned from the `session.created` that
  // announced them. A session not in this set is treated as the root, which is
  // the safe default: on a resumed session the root's own `session.created`
  // fired in an earlier run and will not be seen again, while any child
  // created during this one announces itself here first.
  const childSessions = new Set();

  // Nothing to publish to when the sandbox agent is not listening — a
  // configure sandbox, or opencode run outside a Discobox terminal. The
  // publisher tolerates it, but spawning a process per event to discover that
  // does not.
  const publish = process.env.DISCOBOX_HOOK_SOCKET
    ? async (event, payload) => {
        try {
          const body = new Blob([JSON.stringify(payload ?? {})]);
          // Redirected rather than interpolated: a payload carries arbitrary
          // text — a prompt, a diff, a command line — and stdin is the one
          // path that cannot be confused for shell syntax.
          await $`discobox-hook-publish --provider ${PROVIDER} --event ${event} < ${body}`
            .quiet()
            .nothrow();
        } catch {
          // A harness must run whether or not its hooks are recorded. This is
          // the log, never the behaviour.
        }
      }
    : async () => {};

  return {
    event: async ({ event }) => {
      if (!event?.type || !PUBLISHED_EVENTS.has(event.type)) return;
      const properties = event.properties ?? {};
      // `info.id` first: on a `session.created` it is the session being
      // created, which is the one being classified. The top-level `sessionID`
      // agrees with it on every payload seen so far, but only `info.id` is
      // guaranteed to name the new session, and learning a child under one
      // field while filtering under the other would leak its SessionStart.
      const sessionID = properties.info?.id ?? properties.sessionID;
      if (event.type === "session.created" && properties.info?.parentID && sessionID) {
        childSessions.add(sessionID);
      }
      // A deleted session can never be spoken of again, so this is also what
      // keeps the set from growing for the life of the terminal.
      if (event.type === "session.deleted" && sessionID) {
        childSessions.delete(sessionID);
      }
      if (ROOT_ONLY_EVENTS.has(event.type) && sessionID && childSessions.has(sessionID)) {
        return;
      }
      await publish(event.type, properties);
    },
    // opencode names these before/after a tool runs; they are what a reader
    // waiting on tool use matches, and they carry the canonical PreToolUse and
    // PostToolUse names.
    "tool.execute.before": async (input, output) => {
      await publish("tool.execute.before", {
        tool: input?.tool,
        sessionID: input?.sessionID,
        callID: input?.callID,
        args: output?.args,
      });
    },
    "tool.execute.after": async (input, output) => {
      await publish("tool.execute.after", {
        tool: input?.tool,
        sessionID: input?.sessionID,
        callID: input?.callID,
        title: output?.title,
      });
    },
  };
};
