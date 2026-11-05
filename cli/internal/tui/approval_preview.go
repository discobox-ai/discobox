//go:build prototype

package tui

import (
	"fmt"
	"io"
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
	f := newForm(
		pickRow("credential", "Credential", choice{key: "github", label: "GitHub token", hint: "bearer · api.github.com"}, choice{key: "new", label: "New credential…", hint: "Open the new credential form"}),
		pickRow("duration", "Duration", choice{key: "1h", label: "1 hour", hint: "Requested by the box"}, choice{key: "8h", label: "8 hours"}, choice{key: "1d", label: "1 day"}),
		pickRow("decision", "Decision", choice{key: "approve", label: "Approve"}, choice{key: "deny", label: "Deny"}),
	)
	if req.asksBoth() {
		f.rows = append(f.rows[:2], append([]formRow{pickRow("parts", "Permission", choice{key: "both", label: "Use and delegate"}, choice{key: "use", label: "Use only"}, choice{key: "delegate", label: "Delegate only"})}, f.rows[2:]...)...)
	}
	d := formDialog("Allow GitHub access?", f, func(f *form) tea.Cmd {
		return p.receipt(f.chosen("decision") + " · " + f.chosen("credential") + " · " + f.chosen("duration") + " · " + f.chosen("parts"))
	})
	d.titleRight = "2m ago"
	d.sections = []section{
		{fields: []field{{label: "Box", value: "Fix approval forms · sbx_123", tone: toneAccent}, {label: "Reason", value: "“" + req.Justification + "”"}}},
		previewRouteSection("Requested routes", req.Use),
		{fields: []field{{label: "Variable", value: "GH_TOKEN"}}},
	}
	if p.fixture != nil {
		d.title = "Allow GitHub use and delegation?"
		d.sections = []section{{fields: []field{{label: "Box", value: "Manager · sbx_manager", tone: toneAccent}, {label: "Reason", value: req.Justification}}}, previewRouteSection("Use", req.Use), previewRouteSection("Delegate", req.Delegate), {fields: []field{{label: "Variable", value: req.EnvVar}}}}
		d.footer = "Manager fixture · all routes shown · PgUp/PgDn scroll the request"
	}
	d.keys = []hint{says("↑↓ move"), says("←→ change"), pressing("enter submits preview", "enter"), pressing("esc resets", "esc")}
	d.footer = "Design sketch · credential use · no existing grants in this example"
	if p.fixture != nil {
		d.footer = "Manager fixture · all routes shown · PgUp/PgDn scroll the request"
	}
	p.d = d
}

func previewRouteCount(part *RequestPart) int {
	if part == nil {
		return 0
	}
	return len(part.Routes)
}
