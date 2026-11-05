//go:build prototype

package tui

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// RunApprovalPreview is a local design workbench. It never creates a data source
// or calls an API. The prototype build tag keeps it out of the product binary.
func RunApprovalPreview(out io.Writer, capture bool, current bool, width, height int, requestPath, policyRoot string) error {
	p := &approvalPreview{width: width, height: height, current: current, st: newStyles(true)}
	if requestPath != "" {
		req, err := loadApprovalPreviewRequest(requestPath, policyRoot)
		if err != nil {
			return err
		}
		p.fixture = req
	}
	p.open()
	if capture {
		_, err := fmt.Fprintln(out, p.render())
		return err
	}
	_, err := tea.NewProgram(p, tea.WithOutput(out)).Run()
	return err
}

type approvalPreview struct {
	width, height int
	current       bool
	fixture       *CredentialRequest
	st            *styles
	d             *dialog
	z             zones
}

func (p *approvalPreview) Init() tea.Cmd { return nil }
func (p *approvalPreview) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		switch keyName(msg) {
		case "ctrl+c", "q":
			return p, tea.Quit
		case "f2":
			p.current = !p.current
			p.open()
			return p, nil
		case "f5":
			p.open()
			return p, nil
		}
		previous := p.d
		cmd, closed := previous.update(msg)
		if closed && p.d == previous {
			p.open()
		}
		return p, cmd
	}
	return p, nil
}
func (p *approvalPreview) View() tea.View {
	v := tea.NewView(p.render())
	v.AltScreen = true
	return v
}
func (p *approvalPreview) render() string {
	p.z.reset()
	mode := "PROPOSED"
	if p.current {
		mode = "CURRENT"
	}
	subject := "Credential use"
	if p.fixture != nil {
		subject = fmt.Sprintf("Manager · %d use / %d delegate routes", previewRouteCount(p.fixture.Use), previewRouteCount(p.fixture.Delegate))
	}
	header := p.st.dimText.Render(subject + " · " + mode + " · F2 compare · F5 reset · q quit · sample data only")
	card := p.d.view(p.st, &p.z, p.width, max(p.height-2, 10))
	return header + "\n" + card
}
func (p *approvalPreview) receipt(result string) tea.Cmd {
	p.d = actionsDialog("Preview · "+result, "Sample only. No request was answered.", []action{{key: "back", label: "Back to the design", enabled: true}}, func(string) tea.Cmd { p.open(); return nil })
	return nil
}
func (p *approvalPreview) open() {
	now := time.Now()
	req := CredentialRequest{ID: "sreq_sample", SandboxID: "sbx_123", Name: "github", EnvVar: "GH_TOKEN", Justification: "The task asks me to open a PR", AskedTTL: time.Hour, Created: now.Add(-2 * time.Minute), Use: &RequestPart{Routes: []secretroute.Route{
		{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}, Paths: []string{"/repos/discobox-ai/discobox/issues/**"}},
		{Hosts: []string{"api.github.com"}, Methods: []string{"POST"}, Paths: []string{"/repos/discobox-ai/discobox/issues/635/comments"}},
	}}}
	if p.fixture != nil {
		req = *p.fixture
	}
	if p.current {
		m := &Model{secrets: newSecretList()}
		m.askAboutCredential(req, []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}, {ID: "sec_openai", Name: "OpenAI key", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.openai.com"}}}}})
		p.d = m.dialog
		// Keep the current renderer and menu intact; replace its side effects.
		p.d.action = func(action string) tea.Cmd { return p.receipt(action) }
		return
	}
	p.showPart(req, req.Use == nil, previewDecision{})
}

type previewDecision struct {
	answer, credential, duration, scope string
}

func (p *approvalPreview) showPart(req CredentialRequest, delegate bool, first previewDecision) {
	credential := pickRow("credential", "Credential",
		choice{key: "github", label: "GitHub token", hint: "bearer · api.github.com, github.com"},
		choice{key: "work", label: "Work GitHub token", hint: "Another sample credential"},
		choice{key: "new", label: "New credential…", hint: "Placeholder: credential creation is outside this sketch"},
	)
	duration := pickRow("duration", "Duration", choice{key: "1h", label: "1 hour"}, choice{key: "8h", label: "8 hours"}, choice{key: "1d", label: "1 day"}, choice{key: "forever", label: "Forever", hint: "No expiry; credential and parent lifetime limits still apply"})
	locked := delegate && first.answer == "accept"
	credential.locked, duration.locked = locked, locked
	partForScope := req.Use
	if delegate {
		partForScope = req.Delegate
	}
	hosts := []string{}
	if partForScope != nil {
		for _, r := range partForScope.Routes {
			if !r.Blocked {
				for _, host := range r.Hosts {
					if !slices.Contains(hosts, host) {
						hosts = append(hosts, host)
					}
				}
			}
		}
	}
	slices.Sort(hosts)
	scope := pickRow("scope", "Scope",
		choice{key: "routes", label: "Requested routes", hint: "Keep the requested method, path, GraphQL, MCP and blocked-route restrictions"},
		choice{key: "hosts", label: "Requested hosts", hint: "Any method, path and protocol on: " + strings.Join(hosts, ", ") + ". Request-level blocks are dropped; credential restrictions still apply."},
		choice{key: "anywhere", label: "Anywhere", hint: "No grant restriction. Still limited by credential scope and any parent delegation."},
	)
	f := newForm(credential, duration, scope, pickRow("decision", "Decision", choice{key: "accept", label: "Accept"}, choice{key: "deny", label: "Deny"}))
	if first.credential != "" {
		f.choose("credential", first.credential)
		f.choose("duration", first.duration)
	}
	title, explain := "Allow credential use?", "This box may use the credential on the routes below."
	part := req.Use
	step := "Use"
	if delegate {
		title, explain = "Allow credential delegation?", "This box may give access to boxes it leads, on the routes below. Delegation does not grant its own use."
		part = req.Delegate
		step = "Delegation"
	}
	d := formDialog(title, f, func(f *form) tea.Cmd {
		answer := previewDecision{f.chosen("decision"), f.chosen("credential"), f.chosen("duration"), f.chosen("scope")}
		if answer.answer == "accept" && answer.scope == "hosts" && len(hosts) == 0 {
			f.err = "This part has no open requested hosts; choose Requested routes or Anywhere."
			return nil
		}
		if answer.answer == "accept" && answer.credential == "new" {
			f.err = "New credential creation is a placeholder; choose either sample token."
			return nil
		}
		if !delegate && req.Delegate != nil {
			p.showPart(req, true, answer)
			return nil
		}
		summary := []string{}
		describe := func(label string, a previewDecision) {
			if a.answer == "deny" {
				summary = append(summary, label+": denied")
				return
			}
			summary = append(summary, label+": accepted · "+a.credential+" · "+a.duration+" · scope="+a.scope)
		}
		if delegate && req.Use != nil {
			describe("Use", first)
		}
		describe(step, answer)
		return p.receipt(strings.Join(summary, "; "))
	})
	d.titleRight = "1 of 1"
	if req.asksBoth() {
		d.titleRight = "1 of 2"
		if delegate {
			d.titleRight = "2 of 2"
		}
	}
	box := "Fix approval forms · sbx_123"
	if p.fixture != nil {
		box = "Manager · sbx_manager"
	}
	d.sections = []section{{fields: []field{{label: "Box", value: box, tone: toneAccent}, {label: "Reason", value: req.Justification}}, lines: []line{{text: explain}}}}
	if delegate && req.Use != nil {
		state := "Use denied. Choose a credential and duration for delegation."
		if locked {
			state = "Use accepted. Credential and duration are locked to that decision."
		}
		d.sections = append(d.sections, section{lines: []line{{text: state, tone: toneAccent}}})
	}
	d.sections = append(d.sections, previewRouteSection("Requested "+strings.ToLower(step), part))
	if !delegate {
		d.sections = append(d.sections, section{fields: []field{{label: "Variable", value: req.EnvVar}}})
	}
	d.keys = []hint{says("↑↓ move"), says("←→ change"), pressing("enter confirms decision", "enter"), pressing("esc resets", "esc")}
	d.footer = "Sample only · PgUp/PgDn scroll the request"
	if !delegate && req.Delegate != nil {
		d.footer = "Accept or deny use, then review delegation · sample only"
	}
	d.sections = append(d.sections, section{lines: []line{{text: "Scope applies to this part only. Credential restrictions and any parent delegation still apply.", tone: toneDim}}})
	p.d = d
}

func previewRouteCount(part *RequestPart) int {
	if part == nil {
		return 0
	}
	return len(part.Routes)
}
