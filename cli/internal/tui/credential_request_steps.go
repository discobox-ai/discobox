package tui

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretformat"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// askWhichCredential is the first step of answering a credential request:
// what was asked, by whom and why, and which credential answers it.
//
// Every saved secret is listed: the one the variable already holds first,
// then those that can answer, then the rest, marked ✗. Why one cannot answer
// is said when it is chosen, on a card of its own, rather than beside every
// row. The first that can answer is highlighted, or a new one when none can.
func (m *Model) askWhichCredential(req CredentialRequest, secrets []Secret, held []HeldGrant) tea.Cmd {
	return m.whichCredential(req, secrets, held, "")
}

// Why a secret cannot answer a request (cannotAnswer).
const (
	notCovered = "routes" // its routes do not cover what is asked
	notHeld    = "held"   // the variable already holds another secret in this box
)

// cannotAnswer is why s cannot answer req, or "". The server takes no other
// secret in a variable the box's own bindings deliver (heldBy); a secret whose
// routes do not cover the request would be refused on the requests it was
// approved for. A variable held from the harness config or project takes
// another secret as a replacement shadowing it (requestAnswer.useAs).
func cannotAnswer(s Secret, req CredentialRequest, holder HeldGrant, pinned bool) string {
	if req.Use != nil && pinned && s.ID != holder.SecretID {
		return notHeld
	}
	if uncovered(s, req) != nil {
		return notCovered
	}
	return ""
}

// whichCredential is step 1 with the cursor on at, or on the first secret
// that can answer when at is empty.
func (m *Model) whichCredential(req CredentialRequest, secrets []Secret, held []HeldGrant, at string) tea.Cmd {
	holder, pinned := heldBy(held)
	var ordered []Secret
	if holder.SecretID != "" {
		ordered = append(ordered, heldSecret(secrets, holder))
	}
	for _, fits := range []bool{true, false} {
		for _, s := range secretsForRequest(secrets, req) {
			if s.ID != holder.SecretID && (cannotAnswer(s, req, holder, pinned) == "") == fits {
				ordered = append(ordered, s)
			}
		}
	}
	var items []action
	cursor := -1
	for i, s := range ordered {
		label := "  " + s.Name
		if cannotAnswer(s, req, holder, pinned) != "" {
			label = "✗ " + s.Name
		} else if cursor < 0 {
			cursor = i
		}
		items = append(items, action{key: s.ID, label: label, enabled: true})
	}
	fresh := action{key: "new", label: "  Enter new credential…", enabled: true}
	// A variable held by a secret takes only that secret, which a new one
	// never is.
	if req.Use != nil && pinned {
		fresh.enabled, fresh.why = false, variableOf(req)+" holds “"+heldSecret(secrets, holder).Name+"”"
	}
	items = append(items, fresh)
	if cursor < 0 {
		cursor = len(items) - 1
	}
	for i, item := range items {
		if at != "" && item.key == at {
			cursor = i
		}
	}

	d := actionsDialog("Credential request", m.credentialSentence(req), items, func(key string) tea.Cmd {
		again := func() tea.Cmd { return m.whichCredential(req, secrets, held, key) }
		if key == "new" {
			return m.enterNewCredential(req, secrets, held, holder.SecretID, again)
		}
		i := slices.IndexFunc(ordered, func(s Secret) bool { return s.ID == key })
		if i < 0 {
			return nil
		}
		s := ordered[i]
		if why := cannotAnswer(s, req, holder, pinned); why != "" {
			return m.whyNotCompatible(req, s, why, heldSecret(secrets, holder), again)
		}
		return m.readStanding(req, s, held, again, func(standing, parents []SecretBinding, held []HeldGrant) tea.Cmd {
			ans := requestAnswer{credential: s.Name, secret: s, credentialRoutes: s.Routes, maxTTL: s.MaxTTL, standing: standing, parents: parents, held: held, holderID: holder.SecretID}
			return m.askSummary(req, ans, again)
		})
	})
	d.titleRight = "credential"
	d.answerLabel = "Which credential should it get?"
	// A request on a server other than the one the window opened on is
	// answered there, and says which (ADR 0131 §1).
	if req.Server != "" {
		d.sections = append(d.sections, section{fields: []field{{label: "Server", value: req.Server, tone: toneAccent}}})
	}
	if why := strings.TrimSpace(req.Justification); why != "" {
		d.sections = append(d.sections, section{label: "for the purpose of", lines: []line{{text: why}}})
	}
	d.cursor = d.nextEnabled(cursor, -1)
	d.keys = []hint{says("↑↓ moves"), pressing("Enter chooses", "enter"), pressing("Esc leaves it waiting", "esc")}
	m.withRawRequest(d, req)
	m.dialog = d
	return nil
}

// readStanding reads what the asking box holds of a saved secret before
// anything is decided, so the last step can say what approving replaces:
// the box's own bindings of it, the delegations they were handed on under,
// and what applies in the variable the use part names. back is step 1.
func (m *Model) readStanding(req CredentialRequest, s Secret, held []HeldGrant, back func() tea.Cmd, next func(standing, parents []SecretBinding, held []HeldGrant) tea.Cmd) tea.Cmd {
	if req.SandboxID == "" {
		return next(nil, nil, held)
	}
	m.dialog = statusDialog("Credential request", "reading what the discobox holds of "+s.Name+"…")
	wait := m.dialog
	ctx, ds := m.ctx, m.ds
	return func() tea.Msg {
		bindings, err := ds.Bindings(ctx, req.Server, BindingFilter{SecretID: s.ID})
		var standing []SecretBinding
		for _, b := range bindings {
			if b.TargetKind == BindingSandbox && b.TargetID == req.SandboxID {
				standing = append(standing, b)
			}
		}
		now := held
		if err == nil && req.Use != nil && req.EnvVar != "" {
			now, err = ds.HeldGrants(ctx, req.Server, req.SandboxID, req.EnvVar)
		}
		byID := map[string]SecretBinding{}
		for _, b := range bindings {
			byID[b.ID] = b
		}
		var parents []SecretBinding
		seen := map[string]bool{}
		for _, b := range standing {
			for id := b.ParentID; id != "" && !seen[id]; {
				seen[id] = true
				parent, ok := byID[id]
				if !ok {
					break
				}
				parents = append(parents, parent)
				id = parent.ParentID
			}
		}
		return thenMsg{then: func() tea.Cmd {
			if m.dialog != wait {
				return nil
			}
			if err != nil {
				m.dialog = errorDialog("Credential request", fmt.Sprintf("Cannot read what the discobox holds of %s: %v\n\nThe request is still waiting.", s.Name, err))
				m.dialog.onCancel = back
				return nil
			}
			return next(standing, parents, now)
		}}
	}
}

// enterNewCredential is step 2 for a credential the project does not hold
// yet: its name, filled from the request, and its value. back is step 1.
// Nothing is stored here; the value is held only by this card.
func (m *Model) enterNewCredential(req CredentialRequest, secrets []Secret, held []HeldGrant, holderID string, back func() tea.Cmd) tea.Cmd {
	name := textRow("name", "Name", "what you'll know it by", freeSecretName(secrets, credentialName(req)))
	name.required = "give it a name"
	source := pickRow("source", "Get it by",
		choice{key: "paste", label: "Paste a value"},
		choice{key: "command", label: "Output of a command"})
	value := textRow("value", "Value", "paste it here", "")
	value.masked = true
	value.required = "paste the credential's value"
	value.when = func(f *form) bool { return f.chosen("source") == "paste" }
	command := textRow("command", "Command", "gh auth token", "")
	command.required = "say which command prints it, such as gh auth token"
	command.when = func(f *form) bool { return f.chosen("source") == "command" }
	// The sentinel's format is read from the value as it is typed, or from
	// the command's output once it has run, unless a person sets one (chosen).
	// A command runs only when its format is asked to be seen; its output is
	// kept (ran) so storing the credential does not run it again.
	chosen := ""
	var ran struct{ command, output string }
	sample := func(f *form) string {
		if f.chosen("source") == "command" {
			if ran.command != "" && ran.command == strings.TrimSpace(f.value("command")) {
				return ran.output
			}
			return ""
		}
		return strings.TrimSpace(f.value("value"))
	}
	auto := func(f *form) string {
		if v := sample(f); v != "" {
			return secretformat.Describe(v)
		}
		return ""
	}
	sentinel := formRow{key: "format", label: "Sentinel", hint: "what the box holds in place of the value · Enter changes it"}
	sentinel.show = func(f *form) string {
		switch {
		case chosen != "":
			return chosen
		case auto(f) != "":
			return auto(f)
		case f.chosen("source") == "command":
			return "read from the command's output"
		}
		return "read from the value"
	}
	// Worked out, it is drawn quietly: nothing there needs reading unless
	// somebody chose it.
	sentinel.dim = func(*form) bool { return chosen == "" }
	f := newForm(name, source, value, command, sentinel)
	var d *dialog
	backToForm := func() tea.Cmd { m.dialog = d; return nil }
	openFormat := func() tea.Cmd {
		current := chosen
		if current == "" {
			current = auto(f)
		}
		return m.editSentinelFormat(current, sample(f), backToForm, func(format string) tea.Cmd {
			// The format it would be read as anyway is not a choice: nothing
			// is sent for it, and a later value is read afresh.
			chosen = format
			if format == auto(f) {
				chosen = ""
			}
			m.dialog = d
			return nil
		})
	}
	f.rows[4].open = func() tea.Cmd {
		if f.chosen("source") != "command" || sample(f) != "" {
			return openFormat()
		}
		text := strings.TrimSpace(f.value("command"))
		words, err := refreshcmd.Split(text)
		if err != nil || len(words) == 0 {
			f.cursor, f.err = 3, "say which command prints it before its format can be read"
			f.focus()
			return nil
		}
		m.dialog = statusDialog("Credential request", "running "+refreshcmd.Join(words)+" to read its format…")
		ctx := m.ctx
		return func() tea.Msg {
			out, err := refreshcmd.Run(ctx, words)
			return thenMsg{then: func() tea.Cmd {
				if err != nil {
					m.dialog = errorDialog("Credential request", refreshcmd.Join(words)+" failed: "+err.Error()+"\n\nNothing was stored.")
					m.dialog.onCancel = backToForm
					return nil
				}
				ran.command, ran.output = text, strings.TrimSpace(out)
				return openFormat()
			}}
		}
	}
	f.moveTo(2)
	d = formDialog("Credential request", f, func(f *form) tea.Cmd {
		n := strings.TrimSpace(f.value("name"))
		if secretNameTaken(secrets, n) {
			f.cursor, f.err = 0, "the project already has a credential named "+n
			return nil
		}
		// A new credential is sent to the hosts the request names, and the
		// approval narrows it to what was asked (hosts only).
		ans := requestAnswer{credential: n, fresh: true, value: f.value("value"), credentialRoutes: []secretroute.Route{{Hosts: requestHosts(req)}}, held: held, holderID: holderID}
		if chosen != auto(f) {
			ans.format = chosen
		}
		if f.chosen("source") == "command" {
			words, err := refreshcmd.Split(f.value("command"))
			if err != nil || len(words) == 0 {
				f.cursor, f.err = 3, "the command is words, with quotes around one that holds a space"
				return nil
			}
			// Output already read for its format is the first value; the
			// command runs again only when it has not been run.
			ans.command, ans.value = words, sample(f)
		}
		// Back from the part cards is this card as it was left, value and all.
		return m.askSummary(req, ans, backToForm)
	})
	d.titleRight = "new credential"
	d.body = m.credentialSentence(req)
	d.keys = []hint{pressing("Enter continues", "enter"), pressing("Esc goes back", "esc")}
	m.withRawRequest(d, req)
	d.onCancel = back
	m.dialog = d
	return nil
}

// minSentinelBits is the least randomness a chosen format may mint with.
// Sentinels are how the proxy tells one credential's grants from another's,
// so two must never come out the same.
const minSentinelBits = 64

// checkFormat says what is wrong with a format a person typed, and what is
// worth knowing about one that is right. value is the credential's value,
// when there is one, which a format may not copy and should fit.
func checkFormat(format, value string) (warning string, err error) {
	depth, open := 0, 0
	for i, r := range format {
		switch r {
		case '{':
			if depth > 0 {
				return "", fmt.Errorf("the { at %d is inside another {…}; tokens don't nest", i+1)
			}
			depth, open = 1, i
		case '}':
			if depth == 0 {
				return "", fmt.Errorf("the } at %d closes nothing; a format has no literal braces", i+1)
			}
			depth = 0
			token := format[open+1 : i]
			name, length, ok := strings.Cut(token, ":")
			if !ok {
				return "", fmt.Errorf("{%s} needs a charset and a length, as {alnum:36}", token)
			}
			if !slices.ContainsFunc(secretformat.Charsets(), func(c secretformat.Charset) bool { return c.Name == name }) {
				return "", fmt.Errorf("{%s}: %q is not a charset; the charsets are listed above", token, name)
			}
			if n, err := strconv.Atoi(length); err != nil || n <= 0 {
				return "", fmt.Errorf("{%s}: the length is a whole number above 0", token)
			}
		}
	}
	if depth > 0 {
		return "", fmt.Errorf("the { at %d is never closed", open+1)
	}
	t, err := secretformat.ParseChosen(format)
	if err != nil {
		return "", err
	}
	if t.RandomBits() == 0 {
		return "", fmt.Errorf("it has no {charset:length} token, so every sentinel would be the same")
	}
	if bits := t.RandomBits(); bits < minSentinelBits {
		return "", fmt.Errorf("its {…} tokens give %.0f random bits, and a sentinel needs at least %d so no two come out the same", bits, minSentinelBits)
	}
	value = strings.TrimSpace(value)
	if value != "" {
		// The value's own prefix — what inference keeps, ghp_ or
		// github_pat_ — may be written out; nothing else of it may.
		prefix := ""
		if t, err := secretformat.Parse(secretformat.Describe(value)); err == nil && len(t.Literals()) > 0 && strings.HasPrefix(value, t.Literals()[0]) {
			prefix = t.Literals()[0]
		}
		for _, lit := range t.Literals() {
			at := strings.Index(value, lit)
			if len(lit) >= 8 && at >= 0 && (at != 0 || len(lit) > len(prefix)) {
				return "", fmt.Errorf("the text %q is copied from the value; a format is stored in the clear, so only the value's prefix (%q) may come from it", lit, prefix)
			}
		}
		if !t.Validate(value) {
			warning = "The value itself doesn't have this shape. A tool that checks a token's shape may treat the two differently."
		}
	}
	return warning, nil
}

// editSentinelFormat is the screen a sentinel format is typed on, with the
// syntax and every charset to choose from. back is the form unchanged; done
// takes the format, empty for read-it-from-the-value.
func (m *Model) editSentinelFormat(current, value string, back func() tea.Cmd, done func(string) tea.Cmd) tea.Cmd {
	input := textRow("format", "Format", "read it from the value", current)
	f := newForm(input)
	d := formDialog("Sentinel format", f, func(f *form) tea.Cmd {
		format := strings.TrimSpace(f.value("format"))
		if format == "" {
			return done("")
		}
		if _, err := checkFormat(format, value); err != nil {
			f.err = err.Error()
			return nil
		}
		return done(format)
	})
	d.body = "A sentinel is what the box holds in place of the value; discobox swaps the value in on its way out. It should look like the real thing, so a tool that checks a token's shape accepts it."
	var charsets section
	charsets.label = "charsets"
	for _, c := range secretformat.Charsets() {
		charsets.fields = append(charsets.fields, field{label: c.Name, value: c.Alphabet})
	}
	syntax := section{label: "a format is", lines: []line{
		{text: "literal text, and {charset:length} for that many random characters from a charset"},
		{text: "ghp_{alnum:36}   sk-ant-oat01-{base64url:95}   {hex:64}   {alnum:20}.{base64url:42}", tone: toneAccent},
	}}
	sample, sampled := "", ""
	d.refreshSections = func() {
		format := strings.TrimSpace(f.value("format"))
		check := section{label: "this format"}
		switch warning, err := checkFormat(format, value); {
		case format == "":
			check.lines = []line{{text: "Empty: the format is read from the value.", tone: toneDim}}
		case err != nil:
			check.labelTone = toneAlert
			check.lines = []line{{text: err.Error(), tone: toneAlert}}
		default:
			if sampled != format {
				sample, _ = secretformat.MintSentinel(format)
				sampled = format
			}
			check.fields = []field{{label: "a sentinel", value: sample, tone: toneAccent}}
			if warning != "" {
				check.lines = []line{{text: warning, tone: toneDim}}
			}
		}
		d.sections = []section{syntax, charsets, check}
	}
	d.keys = []hint{pressing("Enter saves", "enter"), pressing("Esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// requestAnswer is what the steps have gathered: the credential, saved or
// new, each part's decision and what it is approved for, and how long. A new
// credential's value is held here, and nowhere else, until it is sent.
type requestAnswer struct {
	// credential names the secret that answers: a saved one (secret) or a
	// new one (fresh) stored from value, or from command's output.
	credential string
	secret     Secret
	fresh      bool
	value      string
	command    []string
	// format is the sentinel format a person chose for a new credential;
	// empty, the server reads it from the value.
	format string
	// credentialRoutes are where the credential itself may be sent: a saved
	// one's routes, a new one's hosts. "Anywhere" on a part is these.
	credentialRoutes []secretroute.Route
	// maxTTL is the longest a saved credential lets a grant last, zero for
	// no limit; ttl is how long this one lasts, zero for forever.
	maxTTL, ttl time.Duration
	// use and delegate are each part's answer, PartApprove or PartDeny, empty
	// for a part the request does not have; useChoice and delegateChoice are
	// what an approved part is approved for.
	use, delegate             string
	useChoice, delegateChoice partChoice
	// standing is the asking box's own bindings of a saved secret, parents
	// the delegations they were handed on under, and held what applies in
	// the use part's variable now: what approving replaces.
	standing, parents []SecretBinding
	held              []HeldGrant
	// holderID is the secret delivering the use part's variable now, if any.
	holderID string
}

// useAs is how an approved use part is granted: as the request asked, except
// that an addition stacks only on the secret delivering the variable (ADR
// 26-11-03-329 §4), so asked with another it is granted as a replacement.
func (ans requestAnswer) useAs(req CredentialRequest) bindingplan.As {
	if req.Use != nil && req.Use.Add && (ans.holderID == "" || (!ans.fresh && ans.holderID == ans.secret.ID)) {
		return bindingplan.Add
	}
	return bindingplan.Replace
}

// additionNote says how an approved use part asked as an addition is granted.
func (ans requestAnswer) additionNote(req CredentialRequest) string {
	if req.Use == nil || !req.Use.Add {
		return ""
	}
	if ans.useAs(req) == bindingplan.Add {
		return "Its own use is asked as an addition: it goes beside what " + variableOf(req) + " holds and replaces nothing."
	}
	return "Its own use is asked as an addition, but " + variableOf(req) + " holds another credential, so with “" + ans.credential + "” it is granted as a replacement, setting that one aside."
}

// approval is the answer as the server takes it. SecretID is filled in by
// whoever stores a new credential.
func (ans requestAnswer) approval(req CredentialRequest) Approval {
	scope := func(c partChoice) (string, []secretroute.Route) {
		switch c.scope {
		case "", "edited":
			return "routes", c.routes
		}
		return c.scope, nil
	}
	sent := Approval{RequestID: req.ID, SecretID: ans.secret.ID, TTLSeconds: lifetime.Seconds(ans.ttl), Use: ans.use, Delegate: ans.delegate}
	if ans.use == PartApprove {
		sent.UseScope, sent.UseRoutes = scope(ans.useChoice)
		sent.UseAs = string(ans.useAs(req))
	}
	if ans.delegate == PartApprove {
		sent.DelegateScope, sent.DelegateRoutes = scope(ans.delegateChoice)
	}
	return sent
}

// askSummary is the request in a line per part: the hosts each would reach
// and how many operations. Accept and Deny answer every part at once; Review
// walks the parts' routes, answering each on its own. back is the step
// before.
func (m *Model) askSummary(req CredentialRequest, ans requestAnswer, back func() tea.Cmd) tea.Cmd {
	var d *dialog
	d = approvalDialog("Credential request", newForm(), func(*form) tea.Cmd {
		switch d.formActions[d.formActionAt].key {
		case "review":
			return m.askParts(req, ans, func() tea.Cmd { m.dialog = d; return nil })
		case "deny":
			ans.use, ans.delegate = partAnswer(req.Use, PartDeny), partAnswer(req.Delegate, PartDeny)
		default:
			ans.use, ans.delegate = partAnswer(req.Use, PartApprove), partAnswer(req.Delegate, PartApprove)
			ans.useChoice, ans.delegateChoice = partChoice{scope: "routes"}, partChoice{scope: "routes"}
		}
		return m.partsAnswered(req, ans)
	})
	d.formActions = []action{
		{key: "accept", label: "Accept", enabled: true},
		{key: "deny", label: "Deny", enabled: true},
		{key: "review", label: "Review…", enabled: true},
	}
	gateUntilSeen(d)
	d.titleRight = "summary"
	d.body = m.credentialSentence(req)
	what := section{label: ans.credential + " would be"}
	if req.Use != nil {
		what.fields = append(what.fields, field{label: "used by this box", value: partSummary(req.Use), tone: toneAccent})
	}
	if req.Delegate != nil {
		what.fields = append(what.fields, field{label: "delegated to its workers", value: partSummary(req.Delegate), tone: toneAccent})
	}
	if note := ans.additionNote(req); note != "" {
		what.lines = append(what.lines, line{text: note, tone: toneDim})
	}
	d.sections = []section{what}
	d.keys = []hint{says("←→ moves"), pressing("Enter chooses", "enter"), pressing("Esc goes back", "esc")}
	m.withRawRequest(d, req)
	d.onCancel = back
	m.dialog = d
	return nil
}

// gateUntilSeen holds a step's decisions until it has been drawn: a key typed
// ahead of a new step never answers it unseen, which is what keeps a
// replacement's consequences from being sent past (ADR 26-11-04-616 §4).
// Unlike the scroll gate, nothing has to be read to its end.
func gateUntilSeen(d *dialog) {
	d.requireEnd, d.requireSeen, d.decisionsBlocked = false, true, true
}

// partAnswer is the decision for a part, or none for a part the request
// does not have.
func partAnswer(part *RequestPart, decision string) string {
	if part == nil {
		return ""
	}
	return decision
}

// partSummary is a part in a line: the hosts it reaches and how many
// operations, an operation being a row of its route table.
func partSummary(part *RequestPart) string {
	rows := approvalRouteCells(part.Routes)
	var hosts []string
	for _, r := range rows {
		if !slices.Contains(hosts, r.host) {
			hosts = append(hosts, r.host)
		}
	}
	n := "1 operation"
	if len(rows) != 1 {
		n = itoa(len(rows)) + " operations"
	}
	return strings.Join(hosts, ", ") + " · " + n
}

// askParts asks about each part the request has, use then delegation, on a
// card of its own; a part the request does not have has no card. back is
// the step before. Each part keeps what it was changed to, so going back to
// it shows the change.
func (m *Model) askParts(req CredentialRequest, ans requestAnswer, back func() tea.Cmd) tea.Cmd {
	useChoice, delegateChoice := &partChoice{scope: "routes"}, &partChoice{scope: "routes"}
	delegate := func(back func() tea.Cmd) tea.Cmd {
		if req.Delegate == nil {
			return m.partsAnswered(req, ans)
		}
		return m.askPart(req, ans, true, delegateChoice, back, func(decision string) tea.Cmd {
			ans.delegate, ans.delegateChoice = decision, *delegateChoice
			return m.partsAnswered(req, ans)
		})
	}
	if req.Use == nil {
		return delegate(back)
	}
	return m.askPart(req, ans, false, useChoice, back, func(decision string) tea.Cmd {
		ans.use, ans.useChoice = decision, *useChoice
		use := m.dialog
		return delegate(func() tea.Cmd { m.dialog = use; return nil })
	})
}

// partChoice is what a part is approved for: the routes it asked for
// ("routes"), any method and path on its hosts ("hosts"), anywhere the
// credential may go ("anywhere"), or routes typed in an editor ("edited",
// with routes). The server takes each as the part decision's scope, the
// edited routes as its routes override.
type partChoice struct {
	scope  string
	routes []secretroute.Route
}

// askPart is one part's card: what it would let the box do, as asked or as
// changed, and approve, deny or modify. next takes the decision.
func (m *Model) askPart(req CredentialRequest, ans requestAnswer, delegate bool, choice *partChoice, back func() tea.Cmd, next func(decision string) tea.Cmd) tea.Cmd {
	part, step, label := req.Use, "its own use", "it may use "+ans.credential+" on"
	body := "Discobox “" + boxLabel(m, req) + "” would use " + ans.credential + " itself, in " + variableOf(req) + "."
	if delegate {
		part, step, label = req.Delegate, "its workers", "its workers may be given it on"
		body = "Discobox “" + boxLabel(m, req) + "” would be able to give " + ans.credential + " to the discoboxes it starts. It could not use it itself."
	}
	again := func() tea.Cmd { return m.askPart(req, ans, delegate, choice, back, next) }
	var d *dialog
	d = approvalDialog("Credential request", newForm(), func(*form) tea.Cmd {
		switch d.formActions[d.formActionAt].key {
		case "deny":
			return next(PartDeny)
		case "modify":
			// Back to the card as it was, focus on Approve rather than on the
			// Modify that left it.
			return m.modifyPart(req, ans, part, step, choice, func() tea.Cmd { d.formActionAt = 0; m.dialog = d; return nil }, again)
		}
		return next(PartApprove)
	})
	d.formActions = []action{
		{key: "accept", label: "Approve", enabled: true},
		{key: "deny", label: "Deny", enabled: true},
		{key: "modify", label: "Modify…", enabled: true},
	}
	gateUntilSeen(d)
	d.titleRight = step
	d.body = body
	d.sections = []section{choiceSection(label, part, *choice, ans)}
	if len(part.Roles) > 0 {
		roles := section{label: "the roles it asked for · the discobox's words"}
		for _, role := range part.Roles {
			roles.lines = append(roles.lines, roleLines(role)...)
		}
		// A role names what was asked; it never stands over routes approved
		// in its place (ADR 26-11-04-616 §4).
		if choice.scope != "routes" && choice.scope != "" {
			roles.lines = append(roles.lines, line{}, line{text: "They describe what it asked for, not the routes above.", tone: toneAlert})
		}
		d.sections = append(d.sections, roles)
	}
	d.keys = []hint{says("←→ moves"), pressing("Enter chooses", "enter"), pressing("Esc goes back", "esc")}
	m.withRawRequest(d, req)
	d.onCancel = back
	m.dialog = d
	return nil
}

// choiceSection is a part's routes as the card shows them: as asked, or as
// changed, the rule saying which.
func choiceSection(label string, part *RequestPart, choice partChoice, ans requestAnswer) section {
	switch choice.scope {
	case "hosts":
		sec := approvalRouteSection(label+" · any path on its hosts", &RequestPart{Routes: []secretroute.Route{{Hosts: secretroute.OpenRouteHosts(part.Routes)}}})
		sec.labelTone = toneAccent
		return sec
	case "anywhere":
		sec := approvalRouteSection(label+" · anywhere the “"+ans.credential+"” credential may be sent", &RequestPart{Routes: ans.credentialRoutes})
		sec.labelTone = toneAccent
		// Every list the credential is held to is its own ceiling; none is
		// flattened into another (cli/internal/tui/REVIEW.md).
		for i, list := range ans.secret.RouteConstraints {
			within := approvalRouteSection("and only within its constraint "+itoa(i+1), &RequestPart{Routes: list})
			sec.lines = append(append(sec.lines, line{}, line{text: "── and only within its constraint " + itoa(i+1), tone: toneDim}), within.lines...)
		}
		return sec
	case "edited":
		sec := approvalRouteSection(label+" · edited", &RequestPart{Routes: choice.routes})
		sec.labelTone = toneAccent
		return sec
	}
	return approvalRouteSection(label, part)
}

// modifyPart offers the other things a part may be approved for. back is the
// part's card unchanged; done is the part's card redrawn with the change.
func (m *Model) modifyPart(req CredentialRequest, ans requestAnswer, part *RequestPart, step string, choice *partChoice, back, done func() tea.Cmd) tea.Cmd {
	hosts := secretroute.OpenRouteHosts(part.Routes)
	items := []action{
		{key: "routes", label: "As requested", enabled: true},
		{key: "hosts", label: "Any path on " + strings.Join(hosts, ", "), enabled: len(hosts) > 0, why: "it asks for no host as a whole"},
		{key: "anywhere", label: "Anywhere the “" + ans.credential + "” credential may be sent", enabled: true},
		{key: "edit", label: "Edit the routes…", enabled: true},
	}
	d := actionsDialog("Modify "+step, "", items, func(key string) tea.Cmd {
		if key == "edit" {
			routes := part.Routes
			if choice.scope == "edited" {
				routes = choice.routes
			}
			return m.editRoutes(req, step, routesForEditor(routes, step, ans.credential), back, func(edited []secretroute.Route) tea.Cmd {
				choice.scope, choice.routes = "edited", edited
				return done()
			})
		}
		choice.scope, choice.routes = key, nil
		return done()
	})
	for i, item := range items {
		if item.key == choice.scope || (choice.scope == "edited" && item.key == "edit") {
			d.cursor = i
		}
	}
	d.keys = []hint{says("↑↓ moves"), pressing("Enter chooses", "enter"), pressing("Esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// thenMsg carries work that finished off the update loop back onto it, to
// change the screen there.
type thenMsg struct{ then func() tea.Cmd }

// routesEditedMsg is the editor coming back with a part's routes.
type routesEditedMsg struct {
	path string
	err  error
	then func(text string, err error) tea.Cmd
}

// routesForEditor is a part's routes as the editor gets them: one to a line, in
// the form `discobox secret create --route` takes, under a comment saying
// so.
func routesForEditor(routes []secretroute.Route, step, credential string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# The routes %s is approved for (%s), one to a line:\n", credential, step)
	b.WriteString("#   [METHODS ]HOSTS[PATHS][ graphql=OPS[:FIELDS]][ mcp=TOOLS][=blocked]\n")
	b.WriteString("# as `discobox secret create --route` takes them. Lines starting with #\n")
	b.WriteString("# are ignored. Save and quit to use them; save it empty to change nothing.\n\n")
	for _, r := range routes {
		b.WriteString(secretroute.Format(r))
		b.WriteString("\n")
	}
	return b.String()
}

// parseRoutes reads routes back from the editor, a refusal naming the line.
func parseRoutes(text string) ([]secretroute.Route, error) {
	var routes []secretroute.Route
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := secretroute.Parse(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		routes = append(routes, r)
	}
	return routes, nil
}

// editRoutes hands text to the editor and reads routes back. Routes that do
// not parse say why, and offer the editor again, on what was saved, or
// abandoning the edit. Saved with no routes is abandoning it. back is the
// part's card unchanged.
func (m *Model) editRoutes(req CredentialRequest, step, text string, back func() tea.Cmd, done func([]secretroute.Route) tea.Cmd) tea.Cmd {
	return editText(m.ctx, text, "routes", func(path string, err error) tea.Msg {
		return routesEditedMsg{path: path, err: err, then: func(saved string, err error) tea.Cmd {
			if err != nil {
				m.dialog = errorDialog("Edit the routes", "The editor failed: "+err.Error()+"\n\nNothing changed.")
				m.dialog.onCancel = back
				return nil
			}
			routes, err := parseRoutes(saved)
			if err == nil && len(routes) == 0 {
				return back()
			}
			if err == nil {
				return done(routes)
			}
			d := actionsDialog("These routes don't parse", err.Error(), []action{
				{key: "again", label: "Edit again", enabled: true},
				{key: "abort", label: "Abort the edit", enabled: true},
			}, func(key string) tea.Cmd {
				if key == "again" {
					return m.editRoutes(req, step, saved, back, done)
				}
				return back()
			})
			d.err = true
			d.keys = []hint{says("↑↓ moves"), pressing("Enter chooses", "enter"), pressing("Esc aborts the edit", "esc")}
			d.onCancel = back
			m.dialog = d
			return nil
		}}
	})
}

// routesEdited reads the editor's file and hands it to whoever asked.
func (m *Model) routesEdited(msg routesEditedMsg) tea.Cmd {
	if msg.err != nil {
		os.Remove(msg.path)
		return msg.then("", msg.err)
	}
	b, err := os.ReadFile(msg.path)
	os.Remove(msg.path)
	return msg.then(string(b), err)
}

// partsAnswered ends the decisions. Every part denied is the request
// denied; otherwise how long is asked last. Back from it is the card that
// decided.
func (m *Model) partsAnswered(req CredentialRequest, ans requestAnswer) tea.Cmd {
	if ans.use != PartApprove && ans.delegate != PartApprove {
		return m.denyCredential(req)
	}
	decided := m.dialog
	return m.askHowLong(req, ans, func() tea.Cmd { m.dialog = decided; return nil })
}

// askHowLong is the last step: how long the box holds what was approved,
// over everything the approval is — the credential, each part's answer and
// what it is approved for, and what it replaces at the lifetime under the
// cursor (ADR 26-11-04-616 §4). It opens on what the box asked for, or an
// hour; a saved credential's limit takes the longer choices away, saying
// why. Choosing one sends the answer.
func (m *Model) askHowLong(req CredentialRequest, ans requestAnswer, back func() tea.Cmd) tea.Cmd {
	asked := req.AskedTTL
	if asked <= 0 || asked%time.Second != 0 {
		asked = agentcreds.DefaultGrantTTL
	}
	limit := "“" + ans.credential + "” credential allows at most " + lifetime.Label(ans.maxTTL)
	var items []action
	cursor := 0
	for i, d := range lifetimeChoices(req.AskedTTL) {
		item := action{key: strconv.FormatInt(lifetime.Seconds(d), 10), label: lifetime.Label(d), enabled: true}
		if d == req.AskedTTL && req.AskedTTL > 0 {
			item.label += " · what it asked for"
		}
		if !withinLimit(Secret{MaxTTL: ans.maxTTL}, d) {
			item.enabled, item.why = false, limit
		}
		if d == asked {
			cursor = i
		}
		items = append(items, item)
	}
	items = append(items, action{key: "custom", label: "Custom…", enabled: true})
	what := ans.credential
	if ans.use != PartApprove {
		what += ", to give to its workers,"
	}
	var d *dialog
	d = actionsDialog("Credential request", "How long should Discobox “"+boxLabel(m, req)+"” hold "+what+"?", items, func(key string) tea.Cmd {
		if key == "custom" {
			return m.customHowLong(ans, limit, func() tea.Cmd { m.dialog = d; return nil }, func(ttl time.Duration) tea.Cmd {
				ans.ttl = ttl
				return m.sendAnswer(req, ans)
			})
		}
		seconds, _ := strconv.ParseInt(key, 10, 64)
		ans.ttl = time.Duration(seconds) * time.Second
		return m.sendAnswer(req, ans)
	})
	d.titleRight = "how long"
	gateUntilSeen(d)
	d.cursor = d.nextEnabled(cursor, -1)
	d.refreshSections = func() {
		ttl, known := time.Duration(0), false
		if d.cursor >= 0 && d.cursor < len(d.items) && d.items[d.cursor].key != "custom" {
			seconds, _ := strconv.ParseInt(d.items[d.cursor].key, 10, 64)
			ttl, known = time.Duration(seconds)*time.Second, true
		}
		d.sections = answerSections(req, ans, ttl, known)
	}
	d.keys = []hint{says("↑↓ moves"), pressing("Enter sends", "enter"), pressing("Esc goes back", "esc")}
	m.withRawRequest(d, req)
	d.onCancel = back
	m.dialog = d
	return nil
}

// customHowLong takes a lifetime typed as the CLI's flags take one.
func (m *Model) customHowLong(ans requestAnswer, limit string, back func() tea.Cmd, done func(time.Duration) tea.Cmd) tea.Cmd {
	input := textRow("ttl", "For", "90m, 3d, 6mo or forever", "")
	input.required = "say how long, such as 90m, 3d, 6mo or forever"
	f := newForm(input)
	d := formDialog("How long", f, func(f *form) tea.Cmd {
		ttl, err := lifetime.Parse(f.value("ttl"))
		if err != nil {
			f.err = err.Error()
			return nil
		}
		if !withinLimit(Secret{MaxTTL: ans.maxTTL}, ttl) {
			f.err = limit
			return nil
		}
		return done(ttl)
	})
	d.keys = []hint{pressing("Enter sends", "enter"), pressing("Esc goes back", "esc")}
	d.onCancel = back
	m.dialog = d
	return nil
}

// answerSections is the whole approval, as the last step shows it above the
// lifetimes: what answers, each part's answer and what it is approved for,
// what it replaces and the delegations it is held within. ttl is the
// lifetime under the cursor, unknown while a custom one is still to be typed.
func answerSections(req CredentialRequest, ans requestAnswer, ttl time.Duration, known bool) []section {
	with := ans.credential
	if ans.fresh {
		with += " · new, stored when this is sent"
	}
	sec := section{label: "this approval", fields: []field{{label: "credential", value: with, tone: toneAccent}}}
	part := func(label, answer string, c partChoice) {
		value := "denied — what it holds of it stays as it is"
		if answer == PartApprove {
			value = "approved · " + choiceText(c)
		}
		sec.fields = append(sec.fields, field{label: label, value: value})
	}
	if req.Use != nil {
		part("its own use", ans.use, ans.useChoice)
		if note := ans.additionNote(req); note != "" && ans.use == PartApprove {
			sec.fields = append(sec.fields, field{label: "", value: note, tone: toneDim})
		}
	}
	if req.Delegate != nil {
		part("its workers", ans.delegate, ans.delegateChoice)
	}
	out := []section{sec}
	if lines := replaces(req, ans, ttl, known); len(lines) > 0 {
		out = append(out, section{label: "this replaces", labelTone: toneAlert, lines: lines})
	}
	if ans.delegate == PartApprove || ans.use == PartApprove {
		for _, parent := range ans.parents {
			within := section{label: "held within its lead's delegation " + parent.ID}
			if len(parent.Routes) == 0 {
				within.lines = []line{{text: "no route restriction of its own", tone: toneDim}}
			} else {
				within.lines = routeRowsLines(parent.Routes)
			}
			out = append(out, within)
		}
	}
	return out
}

// choiceText is what a part is approved for, in a phrase.
func choiceText(c partChoice) string {
	switch c.scope {
	case "hosts":
		return "any path on its hosts"
	case "anywhere":
		return "anywhere the credential may be sent"
	case "edited":
		return plural(len(c.routes), "edited route", "edited routes")
	}
	return "the routes it asked for"
}

// replaces is what approving takes away or cuts short of what the box holds
// (ADR 26-11-03-329 §5, 26-11-04-616 §4), by bindingplan's rules, the ones
// the server applies: for the use part, each grant in the variable that goes,
// is shadowed or is shortened at ttl; for the delegate part, the box's own
// delegation of the secret.
func replaces(req CredentialRequest, ans requestAnswer, ttl time.Duration, known bool) []line {
	var out []line
	if ans.use == PartApprove && ans.useAs(req) == bindingplan.Replace {
		var until time.Time
		if known && ttl > 0 {
			until = time.Now().Add(ttl)
		}
		planned := make([]bindingplan.Held, 0, len(ans.held))
		for _, grant := range ans.held {
			planned = append(planned, bindingplan.Held{ID: grant.BindingID, Adds: grant.Adds, Own: grant.From == BindingSandbox, Expires: grant.Expires})
		}
		fates := map[string]bindingplan.HeldFate{}
		for _, step := range bindingplan.Replacing(planned, until) {
			fates[step.Held.ID] = step.Fate
		}
		for _, grant := range ans.held {
			name := grant.SecretName
			if name == "" {
				name = grant.SecretID
			}
			what := "“" + name + "” in " + variableOf(req)
			switch grant.From {
			case BindingHarnessConfig:
				what += ", from its harness config"
			case BindingProject:
				what += ", from its project"
			}
			span := "the time you choose"
			if known {
				span = lifetime.Label(ttl)
			}
			switch fates[grant.BindingID] {
			case bindingplan.Goes:
				out = append(out, line{text: what + " — goes", tone: toneAlert, bullet: true})
			case bindingplan.Shadowed:
				out = append(out, line{text: what + " — is set aside for " + span, tone: toneAlert, bullet: true})
			case bindingplan.Shortened:
				out = append(out, line{text: what + " — lasts only " + span, tone: toneAlert, bullet: true})
			}
		}
	}
	if ans.delegate == PartApprove {
		useAs := bindingplan.As("")
		if ans.use == PartApprove {
			useAs = ans.useAs(req)
		}
		for _, f := range replacedFields(ans.standing, true, req.EnvVar, useAs, delegatedRoutes(req, ans), ans.secret.Routes) {
			if f.tone == toneDim {
				continue
			}
			out = append(out, line{text: f.value, tone: f.tone})
		}
	}
	return out
}

// delegatedRoutes is the routes an approved delegate part grants.
func delegatedRoutes(req CredentialRequest, ans requestAnswer) []secretroute.Route {
	switch ans.delegateChoice.scope {
	case "hosts":
		return []secretroute.Route{{Hosts: secretroute.OpenRouteHosts(req.Delegate.Routes)}}
	case "anywhere":
		return nil
	case "edited":
		return ans.delegateChoice.routes
	}
	return req.Delegate.Routes
}

// routeRowsLines is routes as lines, one to a row.
func routeRowsLines(routes []secretroute.Route) []line {
	var out []line
	for _, text := range formatRoutes(routes) {
		out = append(out, line{text: text, tone: toneAccent})
	}
	return out
}

// sendAnswer sends it: a new credential is stored first, its value the one
// typed or the command's output (run here only if it has not been), then the
// approval is made with it. A refusal after the store says the credential
// stays.
func (m *Model) sendAnswer(req CredentialRequest, ans requestAnswer) tea.Cmd {
	sent := ans.approval(req)
	status := "approving for " + lifetime.Label(ans.ttl) + "…"
	if ans.fresh {
		status = "storing " + ans.credential + "…"
		if ans.command != nil && strings.TrimSpace(ans.value) == "" {
			status = "running " + refreshcmd.Join(ans.command) + "…"
		}
	}
	m.dialog = statusDialog("Credential request", status)
	ctx, ds := m.ctx, m.ds
	return func() tea.Msg {
		if ans.fresh {
			stored, err := ds.CreateSecret(ctx, req.Server, NewSecret{
				Name:           ans.credential,
				Type:           "token",
				Routes:         ans.credentialRoutes,
				Format:         ans.format,
				RefreshCommand: ans.command,
				Value:          SecretValue{Token: strings.TrimSpace(ans.value)},
			})
			if err != nil {
				return credentialAnsweredMsg{request: req, approved: true, ttl: ans.ttl, err: err}
			}
			sent.SecretID = stored.ID
			if err := ds.ApproveCredentialRequest(ctx, req.Server, sent); err != nil {
				return credentialAnsweredMsg{request: req, approved: true, ttl: ans.ttl, err: err, applied: []string{"the credential was stored as the project secret " + stored.Name}}
			}
			return credentialAnsweredMsg{request: req, approved: true, ttl: ans.ttl}
		}
		err := ds.ApproveCredentialRequest(ctx, req.Server, sent)
		return credentialAnsweredMsg{request: req, approved: true, ttl: ans.ttl, err: err}
	}
}

// whyNotCompatible says why a secret cannot answer the request (cannotAnswer):
// the variable holds another in this box, or where it may be sent does not
// cover what is asked. holding is the secret the variable holds. back is
// step 1.
func (m *Model) whyNotCompatible(req CredentialRequest, s Secret, why string, holding Secret, back func() tea.Cmd) tea.Cmd {
	d := actionsDialog("“"+s.Name+"” can't be used here", "", []action{{key: "back", label: "Choose another credential", enabled: true}},
		func(string) tea.Cmd { return back() })
	switch {
	case why == notHeld:
		d.body = variableOf(req) + " already holds “" + holding.Name + "” in this discobox, and a variable it holds takes no other credential. Answer with “" + holding.Name + "”, or take it back from the discobox on the secrets screen (F4) first."
	case len(s.Routes) == 0:
		d.body = "“" + s.Name + "” has no routes, so discobox never sends it anywhere. This request needs it sent to " + strings.Join(requestHosts(req), ", ") + "."
	default:
		d.body = "Discobox sends “" + s.Name + "” only where its routes allow, and they don't cover everything this request asks for. Approving with it would give the box a credential that is refused on those requests."
		d.sections = append(d.sections, routesSection(s.Name+" may be sent to", s.Routes))
		for i, list := range s.RouteConstraints {
			d.sections = append(d.sections, routesSection("and only within its constraint "+itoa(i+1), list))
		}
		missing := approvalRouteSection("requested, but not covered", &RequestPart{Routes: uncovered(s, req)})
		missing.labelTone = toneAlert
		d.sections = append(d.sections, missing)
	}
	d.keys = []hint{pressing("Enter or Esc goes back", "esc")}
	m.withRawRequest(d, req)
	d.onCancel = back
	m.dialog = d
	return nil
}

// uncovered is the routes the request asks for that the secret may not be
// sent on: every list it is held to — its routes, and each constraint — has
// to cover each requested route. A secret with no routes is sent nowhere and
// covers none. secretroute.ProveContained fails closed, so a route covered
// only by two secret routes together counts as uncovered: a working secret
// may be marked ✗, never the other way round.
func uncovered(s Secret, req CredentialRequest) []secretroute.Route {
	var out []secretroute.Route
	for _, r := range uniqueRoutes(req.routes()) {
		if r.Blocked {
			continue
		}
		lists := append([][]secretroute.Route{s.Routes}, s.RouteConstraints...)
		for _, list := range lists {
			if len(list) == 0 || secretroute.ProveContained(nil, []secretroute.Route{r}, list) != nil {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// routesSection is where a secret may be sent: its hosts on one line when
// every route is a whole host, the approval card's route table otherwise.
func routesSection(label string, routes []secretroute.Route) section {
	var hosts []string
	for _, r := range routes {
		if r.Blocked || len(r.Methods) > 0 || len(r.Paths) > 0 || r.GraphQL != nil || r.MCP != nil {
			return approvalRouteSection(label, &RequestPart{Routes: routes})
		}
		hosts = append(hosts, r.Hosts...)
	}
	return section{label: label, lines: []line{{text: strings.Join(hosts, ", "), tone: toneAccent}}}
}

// uniqueRoutes drops repeats, which a request asking the same route for its
// use and for its delegation has.
func uniqueRoutes(routes []secretroute.Route) []secretroute.Route {
	seen := map[string]bool{}
	var out []secretroute.Route
	for i, text := range formatRoutes(routes) {
		if !seen[text] {
			seen[text] = true
			out = append(out, routes[i])
		}
	}
	return out
}

// requestHosts is the hosts a request names.
func requestHosts(req CredentialRequest) []string {
	var hosts []string
	for _, r := range req.routes() {
		for _, h := range r.Hosts {
			if !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	return hosts
}

// credentialSentence is the request in one sentence: which box, which
// credential, in which variable.
func (m *Model) credentialSentence(req CredentialRequest) string {
	what := credentialName(req)
	if v := variableOf(req); v != "" && v != what {
		what += " (" + v + ")"
	}
	return "Discobox “" + boxLabel(m, req) + "” has asked for a credential, " + what + "."
}

// boxLabel names the box by the name a person gave it, falling back to its
// ID.
func boxLabel(m *Model, req CredentialRequest) string {
	for _, box := range m.list.all {
		if box.ID == req.SandboxID && strings.TrimSpace(box.Name) != "" {
			return box.Name
		}
	}
	return req.SandboxID
}

// withRawRequest lets ctrl+r open the request as the server sent it, from
// any step, and come back to that step as it was left.
func (m *Model) withRawRequest(d *dialog, req CredentialRequest) {
	if req.Raw == "" {
		return
	}
	d.shortcuts = map[string]func() tea.Cmd{"ctrl+r": func() tea.Cmd {
		return m.showRawRequest(req, func() tea.Cmd { m.dialog = d; return nil })
	}}
	d.keys = append(d.keys, pressing("ctrl+r raw request", "ctrl+r"))
}
