package tui

import (
	"fmt"
	"os"
	"slices"
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

// Why a secret cannot answer a request (cannotAnswer).
const (
	notCovered = "routes" // its routes do not cover what is asked
)

// cannotAnswer is why s cannot answer req, or "": a secret whose routes do
// not cover the request would be refused on the requests it was approved
// for. Another secret than the one holding the variable answers too: in a
// variable the box's own binding delivers, approving it rebinds the
// variable, which the cards show first (rebindPreview, ADR 26-11-19-927); one
// held from the harness config or project is shadowed by it
// (requestAnswer.useAs).
func cannotAnswer(s Secret, req CredentialRequest, holder HeldGrant, readable bool) string {
	// The secret the variable holds, when it is not one this window can read
	// (heldSecret names it and nothing more), has routes nobody here can
	// check: it is not said to cover nothing, and the server takes it.
	if s.ID == holder.SecretID && !readable {
		return ""
	}
	if uncovered(s, req) != nil {
		return notCovered
	}
	return ""
}

// commandRanMsg is a new credential's command come back, run to read the
// format of what it prints.
type commandRanMsg struct {
	wait   *dialog
	output string
	err    error
	then   func(output string, err error) tea.Cmd
}

// commandRan hands the output on, unless the wait for it was left: a slow
// command never lands its card over whatever was opened since.
func (m *Model) commandRan(msg commandRanMsg) tea.Cmd {
	if m.dialog != msg.wait {
		return nil
	}
	return msg.then(msg.output, msg.err)
}

// takenNameNotice says that the name a request asked for is the project's
// already, what the new credential will be named instead, and whether the one
// holding that name could answer the request in its place.
func takenNameNotice(req CredentialRequest, secrets []Secret, held []HeldGrant, asked, suggested string) string {
	text := "The project already has a credential named “" + asked + "”, so this one will be named “" + suggested + "” unless you rename it."
	holder, _ := heldBy(held)
	for _, s := range secrets {
		if strings.TrimSpace(s.Name) != strings.TrimSpace(asked) {
			continue
		}
		why := cannotAnswer(s, req, holder, true)
		if why == "" && heldElsewhere(req, held, s.ID) {
			why = "held"
		}
		switch why {
		case "":
			return text + " Esc goes back to choose “" + asked + "” instead."
		case notCovered:
			return text + " “" + asked + "” cannot answer this request: its routes do not cover it."
		}
		return text + " “" + asked + "” cannot answer this request."
	}
	return text
}

// enterNewCredential is step 2 for a credential the project does not hold
// yet: its name, filled from the request, and its value. back returns to approval.
// Nothing is stored here; the value is held only by this card.
func (m *Model) enterNewCredential(req CredentialRequest, secrets []Secret, held []HeldGrant, holderID string, back func() tea.Cmd, done func(requestAnswer) tea.Cmd) tea.Cmd {
	asked := credentialName(req)
	suggested := freeSecretName(secrets, asked)
	name := textRow("name", "Name", "what you'll know it by", suggested)
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
		wait := statusDialog("Credential request", "running "+refreshcmd.Join(words)+" to read its format…")
		wait.onCancel = backToForm
		m.dialog = wait
		ctx := m.ctx
		then := func(out string, err error) tea.Cmd {
			if err != nil {
				m.dialog = errorDialog("Credential request", refreshcmd.Join(words)+" failed: "+err.Error()+"\n\nNothing was stored.")
				m.dialog.onCancel = backToForm
				return nil
			}
			ran.command, ran.output = text, strings.TrimSpace(out)
			return openFormat()
		}
		return func() tea.Msg {
			out, err := refreshcmd.Run(ctx, words)
			return commandRanMsg{wait: wait, output: out, err: err, then: then}
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
		// approval narrows it to what was asked (ADR 26-11-11-857).
		hosts := newCredentialHosts(req)
		if len(hosts) == 0 {
			f.cursor, f.err = 0, "it names no exact host a new credential can be held to — only patterns, or hosts it blocks — so store it on the secrets screen (F4) with the routes it should have"
			return nil
		}
		ans := requestAnswer{credential: n, fresh: true, value: f.value("value"), credentialRoutes: []secretroute.Route{{Hosts: hosts}}, held: held, holderID: holderID}
		if chosen != auto(f) {
			ans.format = chosen
		}
		// A format chosen before the value was typed is held to the value
		// as it now is.
		if ans.format != "" {
			if _, err := checkFormat(ans.format, sample(f)); err != nil {
				f.cursor, f.err = 4, "the sentinel format: "+err.Error()
				return nil
			}
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
		return done(ans)
	})
	d.titleRight = "new credential"
	d.body = m.credentialSentence(req)
	// The card opens on the value, so a name changed to one the project does
	// not hold yet is said where it is read, before anything is stored under
	// it. Going back to the one already named is offered only when the
	// approval card's credential picker lists it: otherwise it is why this
	// card was opened at all.
	if suggested != asked {
		d.body += "\n\n" + takenNameNotice(req, secrets, held, asked, suggested)
	}
	d.keys = []hint{pressing("Enter continues", "enter"), pressing("Esc goes back", "esc")}
	m.withRawRequest(d, req)
	d.onCancel = back
	m.dialog = d
	return nil
}

// checkFormat says what is wrong with a format a person typed, and what is
// worth knowing about one that is right, by the rules every writer of a
// format is held to (secretformat.ParseChosen, CheckValue). value is the
// credential's value, when there is one.
func checkFormat(format, value string) (warning string, err error) {
	t, err := secretformat.ParseChosen(format)
	if err != nil {
		return "", err
	}
	return secretformat.CheckValue(t, value)
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
	// one's routes, or a new one's hosts and any consented placements.
	credentialRoutes []secretroute.Route
	// maxTTL is the longest a saved credential lets a grant last, zero for
	// no limit; ttl is how long this one lasts, zero for forever.
	maxTTL, ttl time.Duration
	// use and delegate are each part's answer, PartApprove or PartDeny, empty
	// for a part the request does not have; useChoice and delegateChoice are
	// what an approved part is approved for.
	use, delegate             string
	useChoice, delegateChoice partChoice
	// standing is the asking box's own bindings of a saved secret, including
	// the parent IDs a replacement must respect, and held what applies in
	// the use part's variable now: what approving replaces.
	standing []SecretBinding
	held     []HeldGrant
	// holderID is the secret delivering the use part's variable now, if any.
	holderID string
	// rebind is, when the box's own binding of another secret holds the
	// variable, what approving with this one rebinds (ADR 26-11-19-927).
	rebind *rebindPreview
}

// canAdd follows the server's rule: additions apply only beside the same
// secret in the variable, or when it holds no secret yet.
func (ans requestAnswer) canAdd() bool {
	return ans.holderID == "" || (!ans.fresh && ans.holderID == ans.secret.ID)
}

// replaceUnavailable names any binding a person's replacement cannot take
// over. The console answers as a person, with no parent delegation.
func (ans requestAnswer) replaceUnavailable() string {
	for _, b := range ans.standing {
		if b.Use && b.ParentID != "" {
			return "binding " + b.ID + " is held under its lead's delegation " + b.ParentID + "; a person's approval cannot replace it"
		}
	}
	return ""
}

func (ans requestAnswer) useAs(req CredentialRequest) bindingplan.As {
	if ans.canAdd() {
		if ans.replaceUnavailable() != "" || ans.useChoice.as == bindingplan.Add || (ans.useChoice.as == "" && req.Use != nil && req.Use.Add) {
			return bindingplan.Add
		}
	}
	return bindingplan.Replace
}

// useMode is what approving the use part does to the rules the box holds of
// the credential: adds beside them, or replaces them (bindingplan.As), and
// whether that differs from what the box asked. A replacement is named by
// the credential, never the asked variable: it makes the approved binding
// the box's whole use of the secret, in every variable (bindingplan.Use).
func (ans requestAnswer) useMode(req CredentialRequest) string {
	mode := ans.useAs(req)
	text := "replaces its rules for " + ans.credential + ", in every variable"
	if mode == bindingplan.Add {
		text = "adds to its rules for " + ans.credential + " · replaces nothing"
	}
	if (mode == bindingplan.Add) != req.Use.Add {
		text += " · edited from the ask"
	}
	return text
}

// delegateMode is what approving the delegate part does: it is always the
// box's whole delegation of the credential (bindingplan.Delegate), never an
// addition beside one it holds.
func (ans requestAnswer) delegateMode() string {
	return "replaces its workers' rules for " + ans.credential
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
		if ans.rebind != nil {
			sent.Replaces = ans.rebind.holder.BindingID
		}
	}
	if ans.delegate == PartApprove {
		sent.DelegateScope, sent.DelegateRoutes = scope(ans.delegateChoice)
	}
	return sent
}

// partAnswer is the decision for a part, or none for a part the request
// does not have.
func partAnswer(part *RequestPart, decision string) string {
	if part == nil {
		return ""
	}
	return decision
}

// singleHTTPRoute gives the compact preview of one exact HTTP operation.
func singleHTTPRoute(routes []secretroute.Route) string {
	if len(routes) != 1 {
		return ""
	}
	r := routes[0]
	if r.Blocked || r.GraphQL != nil || r.MCP != nil || r.Body != nil || r.WebSocket != nil ||
		len(r.Hosts) != 1 || len(r.Methods) != 1 || len(r.Paths) != 1 {
		return ""
	}
	for _, value := range []string{r.Hosts[0], r.Methods[0], r.Paths[0]} {
		if value == "" || strings.Contains(value, "*") {
			return ""
		}
	}
	return secretroute.Format(r)
}

// partChoice is what a part is approved for: the routes it asked for
// ("routes"), any method and path on its hosts ("hosts"), anywhere the
// credential may go ("anywhere"), or routes typed in an editor ("edited",
// with routes). The server takes each as the part decision's scope, the
// edited routes as its routes override.
type partChoice struct {
	as     bindingplan.As
	scope  string
	routes []secretroute.Route
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
	b.WriteString("#   " + secretroute.Syntax + "\n")
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

// partsAnswered sends the decisions at the lifetime selected with the
// credential. Every part denied denies the request instead.
func (m *Model) partsAnswered(req CredentialRequest, ans requestAnswer) tea.Cmd {
	if ans.use != PartApprove && ans.delegate != PartApprove {
		return m.denyCredential(req)
	}
	if ans.fresh {
		return m.reviewNewCredential(req, ans)
	}
	return m.sendAnswer(req, ans)
}

// projectPlacementRoutes keeps only the literal hosts from the original
// request. The returned gaps identify placements this part cannot save.
func projectPlacementRoutes(routes []secretroute.Route, hosts []string) (saved, missing []secretroute.Route) {
	var placements []secretroute.Route
	for _, route := range routes {
		if route.Blocked || (route.Body == nil && route.WebSocket == nil) {
			continue
		}
		placements = append(placements, route)
		candidate, err := secretroute.Normalize(route)
		if err != nil {
			missing = append(missing, route)
			continue
		}
		var excluded []string
		candidate.Hosts = slices.DeleteFunc(candidate.Hosts, func(host string) bool {
			if slices.Contains(hosts, host) {
				return false
			}
			excluded = append(excluded, host)
			return true
		})
		if len(excluded) != 0 {
			gap := candidate
			gap.Hosts = excluded
			missing = append(missing, gap)
		}
		if len(candidate.Hosts) != 0 {
			saved = append(saved, candidate)
		}
	}
	// Blocks affect which route wins. When containment cannot prove the
	// projection safe, omit the whole part's candidates.
	if len(saved) > 0 && secretroute.ProveContained(nil, saved, routes) != nil {
		return nil, placements
	}
	return saved, missing
}

func (m *Model) reviewNewCredential(req CredentialRequest, ans requestAnswer) tea.Cmd {
	var saved, missing []secretroute.Route
	collect := func(part *RequestPart, decision string, choice partChoice) {
		if part == nil || decision != PartApprove {
			return
		}
		eligible, gaps := projectPlacementRoutes(partRoutes(part, choice), ans.credentialRoutes[0].Hosts)
		saved = append(saved, eligible...)
		missing = append(missing, gaps...)
	}
	collect(req.Use, ans.use, ans.useChoice)
	collect(req.Delegate, ans.delegate, ans.delegateChoice)
	if len(saved) == 0 && len(missing) == 0 {
		return m.sendAnswer(req, ans)
	}

	previous := m.dialog
	back := func() tea.Cmd { m.dialog = previous; return nil }
	ans.credentialRoutes = append(slices.Clone(ans.credentialRoutes), uniqueRoutes(saved)...)

	var d *dialog
	d = approvalDialog("Review new credential", newForm(), func(*form) tea.Cmd {
		switch d.formActions[d.formActionAt].key {
		case "save":
			return m.sendAnswer(req, ans)
		case "secrets":
			m.dialog = nil
			server := m.session.serverName(req.Server)
			if server == m.configServer() {
				return m.openSecrets()
			}
			reload := m.applyFilter(server, m.list.folder, m.list.tags)
			if m.secretsOpen {
				return reload
			}
			return tea.Batch(reload, m.openSecrets())
		default:
			return back()
		}
	})
	d.formActions = nil
	d.titleRight = "save and approve"
	storedLabel := "routes saved on the credential"
	d.body = m.credentialSentence(req) + " Save these routes on “" + ans.credential + "” and approve the request for " + lifetime.Label(ans.ttl) + "?"
	if len(missing) != 0 {
		d.titleRight = "placement gaps"
		storedLabel = "eligible routes for the new credential"
		d.body = m.credentialSentence(req) + " The new credential cannot cover all the approved places. Change the part decisions or store a credential with explicit routes in Secrets (F4), then reopen this request and select it."
	} else {
		d.formActions = []action{{key: "save", label: "Save routes and approve", enabled: true}}
	}
	d.formActions = append(d.formActions,
		action{key: "back", label: "Back to decisions", enabled: true},
		action{key: "secrets", label: "Open Secrets (F4)", enabled: true})
	d.formActionAt = len(d.formActions) - 2
	if req.Server != "" {
		d.sections = append(d.sections, section{fields: []field{{label: "Server", value: req.Server, tone: toneAccent}}})
	}
	d.sections = append(d.sections, routesSection(storedLabel, ans.credentialRoutes))
	if req.Use != nil && ans.use == PartApprove {
		d.sections = append(d.sections, choiceSection("used by this box · "+ans.useMode(req), req.Use, ans.useChoice, ans))
	}
	if req.Delegate != nil && ans.delegate == PartApprove {
		d.sections = append(d.sections, choiceSection("delegated to its workers · "+ans.delegateMode(), req.Delegate, ans.delegateChoice, ans))
	}
	if len(missing) != 0 {
		gap := approvalRouteSection("approved places the new credential cannot cover", &RequestPart{Routes: missing})
		gap.labelTone = toneAlert
		d.sections = append(d.sections, gap)
	}
	if lines := replaces(req, ans, ans.ttl, true); len(lines) > 0 {
		d.sections = append(d.sections, section{label: "this replaces", labelTone: toneAlert, lines: lines})
	}
	m.withRawRequest(d, req)
	d.onCancel = back
	m.dialog = d
	return nil
}

func defaultRequestTTL(req CredentialRequest) time.Duration {
	if req.AskedTTL <= 0 || req.AskedTTL%time.Second != 0 {
		return agentcreds.DefaultGrantTTL
	}
	return req.AskedTTL
}

// approvalConsequences shows only information needed at consent: a new
// credential's own ceiling and gaps, and what approving replaces or shortens.
func approvalConsequences(req CredentialRequest, ans requestAnswer) []section {
	var out []section
	if ans.fresh {
		out = append(out, routesSection("new credential stored for", ans.credentialRoutes))
	}
	// Held to exact hosts, a new credential can leave out routes the request
	// names on a pattern: approved, they are never sent on. Said as a saved
	// secret's ✗ says it (uncovered), before it is stored.
	if ans.fresh {
		if missing := gaps(Secret{Routes: ans.credentialRoutes}, approvedRoutes(req, ans)); len(missing) > 0 {
			label := "requested, but this credential will not be sent on"
			for i := range missing {
				if missing[i].Body != nil || missing[i].WebSocket != nil {
					label = "requested routes beyond the new credential’s host routes"
					break
				}
			}
			gap := approvalRouteSection(label, &RequestPart{Routes: missing})
			gap.labelTone = toneAlert
			out = append(out, gap)
		}
	}
	// A rebind is said on the approval card before Accept: a swap of
	// credentials is never one a person finds after (ADR 26-11-19-927 §6).
	if ans.rebind != nil && ans.use == PartApprove {
		out = append(out, ans.rebind.sections(ans)...)
	}
	if lines := replaces(req, ans, ans.ttl, true); len(lines) > 0 {
		out = append(out, section{label: "this replaces", labelTone: toneAlert, lines: lines})
	}

	return out
}

// replaces is what approving takes away or cuts short of what the box holds
// (ADR 26-11-03-329 §5, 26-11-04-616 §4), by bindingplan's rules, the ones
// the server applies: for the use part, each of the box's bindings of the
// secret, in any variable, as bindingplan.Use plans it, the one it keeps
// shortened at ttl, and each other grant in the variable that goes or is
// shadowed; for the
// delegate part, the box's own delegation of the secret.
func replaces(req CredentialRequest, ans requestAnswer, ttl time.Duration, known bool) []line {
	return append(useReplaces(req, ans, ttl, known), delegateReplaces(req, ans)...)
}

// useReplaces is what approving the use part as a replacement takes away or
// cuts short (replaces).
func useReplaces(req CredentialRequest, ans requestAnswer, ttl time.Duration, known bool) []line {
	var out []line
	if ans.use == PartApprove && ans.useAs(req) == bindingplan.Replace {
		var until time.Time
		if known && ttl > 0 {
			until = time.Now().Add(ttl)
		}
		planned := make([]bindingplan.Held, 0, len(ans.held))
		for _, grant := range ans.held {
			planned = append(planned, bindingplan.Held{ID: grant.BindingID, SecretID: grant.SecretID, Adds: grant.Adds, Own: grant.From == BindingSandbox, Expires: grant.Expires})
		}
		fates := map[string]bindingplan.HeldFate{}
		for _, step := range bindingplan.Replacing(planned, ans.secret.ID, until) {
			fates[step.Held.ID] = step.Fate
		}
		for _, grant := range ans.held {
			// The secret's own bindings are said by the plan below.
			if slices.ContainsFunc(ans.standing, func(b SecretBinding) bool { return b.ID == grant.BindingID }) {
				continue
			}
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
			switch fates[grant.BindingID] {
			case bindingplan.Goes:
				out = append(out, line{text: what + " — goes", tone: toneAlert, bullet: true})
			case bindingplan.Rebound:
				out = append(out, line{text: what + " — is rebound to “" + ans.credential + "”", tone: toneAlert, bullet: true})
			case bindingplan.Shadowed:
				text := what + " — is set aside for " + approvalSpan(ttl, known)
				// Another credential taking the variable is said by name
				// (ADR 26-11-19-927 §7).
				if grant.SecretID != ans.secret.ID {
					text += ", while “" + ans.credential + "” delivers it instead"
				}
				out = append(out, line{text: text, tone: toneAlert, bullet: true})
			case bindingplan.Shortened:
				out = append(out, line{text: what + " — lasts only " + approvalSpan(ttl, known), tone: toneAlert, bullet: true})
			}
		}
		// The server replaces the box's use of the secret in every variable
		// (bindingplan.Use), so each of its bindings of it is said by that plan.
		// The one it keeps takes the approval's lifetime, wherever it was.
		if keep := bindingplan.Use(planBindings(ans.standing), req.EnvVar).Keep; keep != "" {
			for _, b := range ans.standing {
				if b.ID != keep {
					continue
				}
				if steps := bindingplan.Replacing([]bindingplan.Held{{ID: b.ID, SecretID: ans.secret.ID, Own: true, Expires: b.Expires}}, ans.secret.ID, until); len(steps) > 0 && steps[0].Fate == bindingplan.Shortened {
					out = append(out, line{text: "“" + ans.credential + "” in " + variableOf(req) + " — lasts only " + approvalSpan(ttl, known), tone: toneAlert, bullet: true})
				}
			}
		}
		for _, f := range replacedFields(ans.standing, false, req.EnvVar, bindingplan.Replace, partRoutes(req.Use, ans.useChoice), ans.secret.Routes) {
			if f.tone == toneDim {
				continue
			}
			out = append(out, line{text: f.value, tone: f.tone})
		}
	}
	return out
}

// delegateReplaces is what approving the delegate part takes of the box's
// own delegation of the secret (replaces), after the use part if that is
// approved too.
func delegateReplaces(req CredentialRequest, ans requestAnswer) []line {
	var out []line
	if ans.delegate == PartApprove {
		useAs := bindingplan.As("")
		if ans.use == PartApprove {
			useAs = ans.useAs(req)
		}
		for _, f := range replacedFields(ans.standing, true, req.EnvVar, useAs, partRoutes(req.Delegate, ans.delegateChoice), ans.secret.Routes) {
			if f.tone == toneDim {
				continue
			}
			out = append(out, line{text: f.value, tone: f.tone})
		}
	}
	return out
}

// approvalSpan is how long an approval lasts as a card says it: ttl when it is
// known, else the time still to be chosen.
func approvalSpan(ttl time.Duration, known bool) string {
	if known {
		return lifetime.Label(ttl)
	}
	return "the time you choose"
}

// approvedRoutes is the routes the answer grants: each approved part's, as
// it is approved for — what a new credential is checked against. A denied
// part grants none, and one approved anywhere is the credential's own routes,
// which never leave a gap.
func approvedRoutes(req CredentialRequest, ans requestAnswer) []secretroute.Route {
	var out []secretroute.Route
	if req.Use != nil && ans.use == PartApprove {
		out = append(out, partRoutes(req.Use, ans.useChoice)...)
	}
	if req.Delegate != nil && ans.delegate == PartApprove {
		out = append(out, partRoutes(req.Delegate, ans.delegateChoice)...)
	}
	return out
}

// partRoutes is the routes a part grants as it is approved for, as the server
// maps a decision's scope (partDecision): the routes it asked for, its open
// hosts, the routes edited for it, or none of its own — anywhere is the
// credential's routes.
func partRoutes(part *RequestPart, c partChoice) []secretroute.Route {
	switch c.scope {
	case "hosts":
		return []secretroute.Route{{Hosts: secretroute.OpenRouteHosts(part.Routes)}}
	case "anywhere":
		return nil
	case "edited":
		return c.routes
	}
	return part.Routes
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

// uncovered is the routes the request asks for that the secret may not be
// sent on: every list it is held to — its routes, and each constraint — has
// to cover each requested route. A secret with no routes is sent nowhere and
// covers none. secretroute.ProveContained fails closed, so a route covered
// only by two secret routes together counts as uncovered: a working secret
// may be marked ✗, never the other way round.
func uncovered(s Secret, req CredentialRequest) []secretroute.Route {
	return gaps(s, req.routes())
}

// gaps is the routes the secret may not be sent on, of those given: uncovered's
// rule over any list of routes.
func gaps(s Secret, routes []secretroute.Route) []secretroute.Route {
	var out []secretroute.Route
	lists := append([][]secretroute.Route{s.Routes}, s.RouteConstraints...)
	for _, r := range uniqueRoutes(routes) {
		if r.Blocked {
			continue
		}
		covered := true
		for _, piece := range routePieces(r) {
			for _, list := range lists {
				if len(list) == 0 || secretroute.ProveContained(nil, []secretroute.Route{piece}, list) != nil {
					covered = false
				}
			}
		}
		if !covered {
			out = append(out, r)
		}
	}
	return out
}

// routePieces is a route split into one route per host and path, each the
// same in every other respect, so a route naming two hosts is covered by a
// secret's two routes, one for each: ProveContained fits a route to one
// route of the list, never to several together.
func routePieces(r secretroute.Route) []secretroute.Route {
	paths := r.Paths
	if len(paths) == 0 {
		paths = []string{""}
	}
	var out []secretroute.Route
	for _, h := range r.Hosts {
		for _, p := range paths {
			piece := r
			piece.Hosts = []string{h}
			piece.Paths = nil
			if p != "" {
				piece.Paths = []string{p}
			}
			out = append(out, piece)
		}
	}
	return out
}

// routesSection is where a secret may be sent: its hosts on one line when
// every route is a whole host, the approval card's route table otherwise.
func routesSection(label string, routes []secretroute.Route) section {
	var hosts []string
	for _, r := range routes {
		if r.Blocked || !r.HostWide() {
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

// newCredentialHosts is the hosts a credential stored to answer req is held
// to (ADR 26-11-11-857): the exact names an open route of the request names,
// never one it only blocks (ADR 26-11-04-616 §1), and never a pattern — *
// would leave the credential no ceiling of its own, and *.com nearly none,
// with nothing here to tell a public suffix from a company's. None is no new
// credential.
func newCredentialHosts(req CredentialRequest) []string {
	return slices.DeleteFunc(secretroute.OpenRouteHosts(req.routes()), func(h string) bool { return strings.Contains(h, "*") })
}

// credentialSentence is the request in one sentence: which box, which
// credential, in which variable.
func (m *Model) credentialSentence(req CredentialRequest) string {
	what := credentialName(req)
	if v := variableOf(req); v != "" && v != what {
		what += " (" + v + ")"
	}
	return "Discobox “" + boxLabel(m, req) + "” asked for “" + what + "”."
}

// boxLabel names the asking box as a person named it (recordTitle), never by
// the title its own program sets: the box does not get to word the card that
// decides what it receives. It is matched on the request's server as well as
// its ID, since two servers' boxes can share one, and falls back to the ID.
func boxLabel(m *Model, req CredentialRequest) string {
	for _, box := range m.list.all {
		if box.ID == req.SandboxID && m.session.serverName(box.Server) == m.session.serverName(req.Server) {
			return recordTitle(box)
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
