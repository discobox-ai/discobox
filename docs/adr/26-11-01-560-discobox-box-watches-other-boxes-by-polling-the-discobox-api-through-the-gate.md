# 26-11-01-560 — `discobox-box` watches other boxes by polling the discobox API through the gate

- **Status**: Accepted
- **Date**: 2026-11-01
- **Amends**: [26-10-31-073](26-10-31-073-a-box-runs-the-monitors-its-agent-enables-and-queues-their-events-until-it-pops-them.md)
  §9, the image's built-in monitors, by one.
- **Relates to**: [26-11-01-477](26-11-01-477-github-repo-is-a-go-built-in-that-probes-before-it-reads-and-an-instance-may-choose-its-interval.md)
  §1, the built-in seam it runs on;
  [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
  §9, what a box with `discoboxApi` may call;
  [26-10-29-316](26-10-29-316-sandbox-state-changes-are-a-resumable-event-stream.md),
  the sandbox event stream;
  [26-10-16-424](26-10-16-424-a-terminals-status-is-what-its-program-reports-over-osc-7501.md),
  a terminal's status.

## Context

A lead discobox running `orchestrate-issues` watches its workers with
`orchestrate.sh`: it polls `discobox admin box ls` for their state, takes a
worker as idle from its terminal's title and screen text, and lists pending
secret requests to approve them (#427). The loop dies with the harness
session. Monitors replace it, and the lead's box-side half needs one that
watches boxes.

What a box can read (880 §9), when a person has set `discoboxApi` on it:

- **`list-sandboxes`**: every box in its project, with each one's
  `displayState`, tags, and the status its primary terminal's program last
  reported (424), in `runtime.agentStatus`.
- **`get-sandbox`**, a terminal's screen and its events: only for the boxes
  it created.
- **`list-secret-requests`**: only the requests of the boxes it created.
- **The sandbox event stream** (316): refused. A sandbox principal is
  refused it "until a consumer needs it scoped to the discoboxes it
  created" (316, Consequences).

## Decision

`discobox-box` is a built-in written in Go on 477 §1's seam: the image's
`50-discobox-box.sh` execs `discobox-sandbox-agent monitor-builtin
discobox-box`.

1. **It polls.** Each run is one `list-sandboxes` and, when requests are
   watched, one `list-secret-requests`, every 30s by default and no less
   than 10s (477 §2's `interval` option). It reports what differs from the
   last run, keeping the last state of each box in its state directory.
2. **It calls the API as the box's `discobox` CLI does**: at
   `DISCOBOX_API_URL`, through the pool's gate, which adds the pool's own
   credential. The box holds none. Without `discoboxApi`, the server's
   refusal is the instance's failure, in its words.
3. **A box is named by `id`, selected by `tag`, or both.** Tags alone are
   the list's own filter. With ids, the list is the project's and both are
   matched in the monitor, by `pkg/sandboxmeta`'s selectors, the server's.
4. **What it reports**, each keyed per box so an unpopped event is replaced
   by the next of its kind:
   - its `displayState` changing (`box.state`), a box created since the
     baseline (`box.created`), and a box named by id leaving the project
     (`box.gone`);
   - its primary terminal's program reporting a new state (`box.busy` for
     `working`, `box.idle` for the rest: idle, done, blocked with its kind,
     error), from the reported status, never from screen text;
   - a secret request from it pending (`box.request`) and answered
     (`box.request.answered`), for the boxes this box created.
5. **Baselines are 073 §3's, for what was already there.** The first run
   records and reports nothing. A box that existed at the baseline and is
   selected later, by its tag or by options changed in place, is recorded
   quietly. A box created after the baseline is news whenever it is first
   seen, since a worker tags itself once it runs, often after its first
   request. A request made after the baseline is news from any watched
   box; one made before it is where things stood. An instance watching
   only ids ends once every one is gone.

## Alternatives rejected

- **Read the sandbox event stream.** It pushes each change once, with no
  poll interval, but it carries no secret requests or tags, and a box is
  refused it. Opening it to a box means scoping it to the boxes it created,
  a change to what a box may see that 316 left for its own decision. A
  `list-sandboxes` every 30s is one request for the whole project; revisit
  when a lead watches enough boxes, or needs changes fast enough, that the
  poll costs more than that decision.
- **Read each box with `get-sandbox`.** A request per box per run, and
  refused for any box the caller did not create, so a lead could not watch
  a peer it was told to.
- **Infer idle and busy from the screen**, as `orchestrate.sh` does. 424 §3
  makes the program's report the status; a screen's wording is not a
  contract.

## Consequences

- A box running `discobox-box` calls the server. `sandbox-agent`'s rule
  against calling back to the server is the agent's own: a monitor is a
  process of the box, run as its user, reaching the API through the gate as
  `discobox` does, not the agent fetching its configuration.
- A request from a box this box did not create is never reported, and the
  skill says so.
- `orchestrate-issues` can watch its workers with `discobox-box -o
  tag=…`, and stop scraping screens and polling `box ls`.
