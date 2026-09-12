# 0097 — A Discobox address is `discobox://<peer-id>`, and nothing user-facing says iroh

- **Status**: Accepted
- **§1's exact-concatenation address superseded by**: [0116](0116-a-discobox-address-names-a-server-and-a-discobox.md) — an address may also name a server by host, and a discobox on it; resolution moves from `Parse` to a `Resolve` step before the dial. The peer-ID format, and §§3-7, stand.
- **Date**: 2026-09-08

## Context

This is what a server prints today when it listens on iroh:

```
iroh endpoint ID is 6ebfa69c6f71c5f4948a0ac6eabe40ec5bc261cb799d61462e5aaf4421cee2ec
listening on iroh://6ebfa69c...cee2ec?addr=172.17.0.1%3A38063&addr=172.18.0.8%3A38063&addr=172.19.0.1%3A38063&addr=172.20.0.1%3A38063&addr=172.21.0.1%3A38063&addr=127.0.0.1%3A38063&addr=%5B%3A%3A1%5D%3A50574
or dial it with the ticket endpointabxl7ju4n5y4l5eurifmn2v6idwfxqtbzn4z2ykgfznk6rbbz3roybybab7qaaabv6uqeaiavqiqaanpvebacafmciaarl5jaiaqblataaa27kicaeakyfaaagx2saqbacwbkaabv6uqeaibaaaaaaaaaaaaaaaaaaaaaaaaaghiway
openapi spec available at iroh://6ebfa69c...?addr=...×7/openapi.yaml
api docs available at iroh://6ebfa69c...?addr=...×7/docs
```

Five of those seven addresses are this host's Docker bridges and the other two
are loopback. None is reachable by the peer the address is being handed to, all
of them describe the host's internal network to anyone the log is pasted to,
and the whole list appears four times.

[ADR 0052](0052-iroh-is-an-optional-endpoint-scheme.md) §6 required the
complete address because it assumed there was no discovery service — "resolving
one needs a discovery service, and this deployment has none until the relays
do" — and it wrote its own condition for revisiting: "once discovery is
deployed the bare `iroh://<endpoint-id>` resolves and the parameters become an
optimization". That condition is met and was probably met on arrival: every
shipped build binds `presets::N0`, which carries n0's DNS and pkarr address
lookup, so a bare endpoint ID resolves globally today.

Shortening the address to `iroh://<endpoint-id>` would fix the noise and leave
the more durable problem. An address is the thing a user writes in a runbook,
pastes into a chat, and puts in a `--server` flag that outlives several of our
decisions. `iroh://` makes every one of those a promise that this server is
reached by iroh, which is an implementation detail of ours that we chose twice
already (ADR 0053, ADR 0067) and may choose differently again.

## Decision

### 1. A peer ID is written `d1-` and lowercase Crockford base32 with check digits

    d1-dtztd73-fe72z95-54a1b3e-nfj0xh3-dw4rebf-6ep2hhn-ebaqm88-eewbp0v

A peer ID is the same value an iroh endpoint ID is — §7 has the naming — and
this is how it is written. Its 32 bytes of ed25519 public key are encoded in
[Crockford base32](https://www.crockford.com/base32.html) — 52 symbols — then
split into four groups of 13, each carrying a Luhn mod-32 check symbol, giving
56. It is displayed lowercase in eight groups of seven.

**The dashes and the case are display only.** Crockford ignores hyphens and is
case-insensitive, so every one of these is the same ID and all of them parse:

    d1-dtztd73-fe72z95-54a1b3e-nfj0xh3-dw4rebf-6ep2hhn-ebaqm88-eewbp0v
    d1dtztd73fe72z9554a1b3enfj0xh3dw4rebf6ep2hhnebaqm88eewbp0v
    D1-DTZTD73-FE72Z95-54A1B3E-NFJ0XH3-DW4REBF-6EP2HHN-EBAQM88-EEWBP0V

That is the property that makes the format worth its length, because it means
we never have to choose between a form that reads well and a form that survives
being pasted. We print the readable one; a terminal that wrapped it, a person
who typed it in caps, and a script that stripped the dashes all still name the
same server.

This does **not** make the address shorter, and it is worth being plain about
that. Displayed it is 77 characters against 71 for `iroh://<hex>` today, and 69
without the dashes. What it buys is different:

- **Confusable characters cannot be written.** Crockford omits `I`, `L`, `O`
  and `U`, and decodes `I`/`l` as `1` and `O` as `0` for anyone who types them
  anyway. The errors check digits exist to catch mostly cannot be made.
- **A typo is caught before it is dialed.** Transposing two characters fails
  its group's check symbol, so it is rejected locally with an error naming the
  address, rather than becoming a plausible ID that connects to nothing and
  surfaces as a timeout somewhere unrelated.
- **It can be read aloud and typed.** Which is what an operator does when the
  server is on a machine they are logged into and the client is not.

Hex was rejected for the second and third of those. It has none of the
tolerance: one wrong character in 64 is a different, valid-looking identity,
with nothing to catch it before the connection that will not work.

`d1` is a format version, and it is part of the **ID**, not decoration on the
URI. An `authorized_ids` file, an API value and an address all need to say
which format they are in, or a later `d2` — a different encoding, a different
transport, an address that carries a relay hint — cannot be told from a `d1` by
anything reading them. Because the prefix is part of the ID, the address is
exact concatenation: `discobox://` followed by the ID, with nothing in between.

### 2. It resolves to the iroh transport at `Parse`, and nothing below `Parse` changes

`Parse` returns `Scheme: "iroh"` and `Value: <endpoint-id>`, keeping the
`discobox://` spelling in `Raw` for display. `Listen`, `HTTPClient`,
`AutoLaunchable` and `DirectlyDialable` are untouched, and so is every caller
of them.

Carrying a `discobox` scheme down through the stack was rejected: it would add
an arm to every scheme switch for a spelling that resolves to a transport those
switches already handle. The scheme is a naming decision, and naming decisions
belong at the edge where names are parsed.

`discobox://` bare is the listen form, exactly as `iroh://` bare is — the
identity comes from the server's key file, so there is nothing about the
address to configure.

### 3. The address is one line, and the fallback is the next one

`Listen` returns `discobox://` plus the ID, with no `?addr=` parameters. That
is the address: what the startup log prints, what the openapi and docs lines
are built from, and what an operator copies and enrolls.

Beside it, the server logs the same address with its direct socket addresses
attached, labelled as the way in without discovery:

    listening on discobox://d1-1mf9ksy-…
    without discovery, dial discobox://d1-1mf9ksy-…?addr=127.0.0.1:38063&addr=…

Both, not one. An earlier draft of this section printed only the plain address,
on the reasoning that discovery resolves it and the socket addresses are almost
always this host's Docker bridges. Verification found the hole in that: with no
route to a discovery service the printed address cannot be dialled, and because
nothing else surfaced the addresses, the `?addr=` escape hatch this section
points at could not be *written* by anyone who needed it. `DirectAddrs` existed
and nothing exposed it. A fallback nobody can obtain is not a fallback.

Discovery is still expected and still the normal path; this is the line an
operator needs on the day it is not available, and it costs one line of log
that says what it is for.

`Parse` accepts `?addr=` on any address, which is what makes the second line
usable and is unchanged.

### 4. The ticket is removed

`IrohTicket`, `parseIrohTicket` and `irohTicketPrefix` go, along with the
`irohTicket` helper in `internal/server`.

The ticket existed because the address was too long and too punctuated to
paste. It carried an endpoint ID and direct addresses in one opaque token — and
in our hands not even a relay, since we never set one and `parseIrohTicket`
discarded any relay a ticket named. So it was a second encoding of exactly what
the URL said, justified entirely by the URL being unpasteable. The address is
now one token with no query string, in an encoding that tolerates being
mangled, and the justification is gone with it.

The cost is real and smaller than it first looks: a discovery-less deployment
loses the single mangle-proof token and keeps the `?addr=` form, which is what
the ticket encoded and which §3 now prints for it. What is actually lost is the
ability to paste that form somewhere that mangles `&` and `%3A`. Revisit by
versioning the address — a `d2` that carries addresses — rather than by
reintroducing a parallel encoding, because two spellings of one address is the
thing this ADR is removing.

### 5. This is how a peer ID is written everywhere

The same string appears in the address, in `discobox admin peer id`, in
`<data dir>/authorized_ids`, and in the `/peers` API (§7 has the naming) —
everywhere a peer ID is read. There is no second spelling for the same thing,
which is what
[ADR 0095](0095-an-enrolled-iroh-id-is-a-managed-resource.md) §2 was reaching
for when it made the endpoint ID the resource's own identity — an operator
should be able to compare what a server printed against a line in a file
without decoding either.

That amends ADR 0095 §2, which named hex. Nothing has shipped against it, so it
is amended rather than superseded, and the change is a spelling: the ID is
still the resource's identity and still its primary key.

The database is the exception, and it is storage rather than a second
spelling: rows are keyed on the compact lowercase form — no dashes — so a
prefix match for `discobox admin peer rm` is a prefix of one stable string
rather than of whatever hyphenation somebody pasted. Everything that reads an
ID normalizes before it stores or compares, and everything that hands one back
renders the written form, so the stored spelling never reaches a reader. An
operator comparing what `peer id` printed against what `peer ls` shows must see
the same characters, or §5 has bought nothing. The store's charset check, which ADR 0095 §2 set
to `^[0-9a-f]{1,64}$` to keep a LIKE wildcard out of a delete, becomes the
Crockford alphabet with the same purpose and the same argument.

### 6. This is the only form, and it is enforced

Hex is not accepted anywhere: not in `authorized_ids`, not in the API, not from
the CLI, and not in `iroh://<id>`. A peer ID has exactly one textual form and
everything that reads one rejects everything else.

This breaks compatibility deliberately. What breaks is an existing
`authorized_ids` file, an `iroh://<hex>` in somebody's notes or systemd unit,
and any `DISCOBOX_SERVER_LISTEN` naming a peer by hex. Each is a line an
operator rewrites once.

Accepting both was the obvious alternative and is what an earlier draft of this
ADR decided. It was rejected because two accepted spellings is the thing §5
exists to remove: an operator comparing what a server printed against a line in
a file has to know that two unrelated-looking strings are the same identity,
error messages have to describe both, and the "one written form" property is
true only of what we emit rather than of what exists. A format that is enforced
is a format; one that is merely preferred is a convention.

**A line that does not parse must be logged.** `parseAuthorizedIDs` skips a
malformed line silently today, on ADR 0052 §5's reasoning that one typo should
not refuse every other enrolled ID — which is right, and which becomes a trap
the moment every existing line stops parsing. An operator would upgrade, find
their break-glass access gone, and have nothing anywhere to tell them why. So
the tolerance stays and the silence goes: a skipped line is logged with the
file, the line number, and what a peer ID looks like. This is the only part of
the change that is about the upgrade rather than the format, and it is the part
that decides whether the upgrade is survivable.

`iroh://` remains as the transport-level spelling of an endpoint (§7), and
takes a `d1` peer ID like everything else. It is not deprecated; it is simply
not a second ID format.

### 7. The thing an ID identifies is a peer

`discobox admin iroh id|ls|add|rm` becomes `discobox admin peer id|ls|add|rm`.
`/iroh-ids` becomes `/peers`, `model.IrohID` becomes `model.Peer`, its table
becomes `peers`, and `internal/resources/irohids` becomes `.../peers`. None of
that has shipped, so nothing is migrated.

This is §2's argument one level up. An operator managing who may reach their
server is doing something about Discobox, not about iroh, and a command named
for the transport makes every runbook that uses it a claim about a transport we
have already reconsidered twice.

"Peer" is not a coinage for this. It is the word the code already uses for
exactly this thing — `endpoint.IrohID` is documented as "the identity an iroh
**peer** is addressed by", `IrohConfig.Authorize` "decides whether a **peer**
may connect", and an unenrolled one "never reaches the handler surface". The
only thing that was not already called a peer was the command.

`device` was rejected: it is Syncthing's word and would be familiar, but a
client here is as often a container, a CI runner, or a second checkout on the
same laptop, none of which is a device. `node` was rejected because iroh itself
called these node IDs before renaming them to endpoint IDs, so it carries
transport flavour and a stale version of it. `access` reads best of all for the
allowlist — `access grant`, `access revoke` — and has no sensible form for
"print this machine's own ID", which would split a four-verb command group in
two to gain nothing.

What keeps the name `iroh` is what *is* iroh: the `iroh://` scheme, the
`iroh.relayUrls` setting and `--iroh-relay` flag, the `irohd` package, and
`endpoint.IrohID`. That is the same boundary §2 draws — the transport keeps its
name where you are configuring the transport, and the domain gets a neutral one
where you are managing who can reach you. A relay in particular is an iroh
concept that a `d2` may not have.

`<data dir>/authorized_ids` is unchanged. It never said iroh, and the IDs in it
are what its name says they are.

`discobox admin iroh` and `discobox admin iroh-id` remain as hidden aliases, for
[ADR 0095](0095-an-enrolled-iroh-id-is-a-managed-resource.md) §5's reason: they
are what an operator's notes say, and breaking them to tidy a command tree is a
cost paid by users.

## Consequences

- Existing `authorized_ids` files, and any `iroh://<hex>` written down
  anywhere, stop working and are rewritten once. A skipped line says so in the
  log rather than silently granting nothing.
- A server prints its address and, beneath it, the same address carrying this
  host's socket addresses for a peer with no discovery. Two lines, and the
  second says which one it is.
- The address a user is handed is 77 characters displayed, 69 compact, one
  token, and says nothing about how it is implemented. It is longer than the
  `iroh://<hex>` it replaces; what it buys is tolerance of hyphens, case and
  the confusions Crockford removes, plus a typo caught locally rather than as a
  timeout later.
- A peer ID has one written form across the address, the log, the CLI, the
  `authorized_ids` file, the API and the database, so two of them can be
  compared by eye.
- Nothing an operator types or reads says "iroh" unless they are configuring
  the transport itself. What transport carries a connection becomes a thing
  they can stop knowing.
- A server's log stops describing the host's Docker bridge topology to anyone
  the log is shared with.
- `endpoint` loses a public function (`IrohTicket`) and the ticket branch of
  `Parse`. Both are used only by this repository — the server logged the
  ticket, and the tests exercised the round trip.
- A discovery-less deployment now writes `?addr=` parameters into the endpoint
  it dials rather than pasting a ticket. That is a worse experience for a
  deployment we do not currently have, and the version prefix is how it gets a
  better one.
- Nothing below `Parse` learns a new scheme, so a future transport is a new
  `d`-version and a new `Parse` arm, not a change to how servers listen or
  clients dial.
