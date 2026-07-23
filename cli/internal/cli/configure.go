package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// newConfigureCommand launches a small inline TUI for managing the project's
// coding agents (harnesses): enable/reconfigure, disable, and set the default.
// It is a focused, inline (non-fullscreen) alternative to the `box harnesses`
// subcommands and the full `tui` dashboard.
func (a *App) newConfigureCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "configure",
		Aliases: []string{"config", "conf", "c", "init"},
		Short:   "Enable, disable, and set the default coding agent",
		Long: `Launch a small inline menu of the project's coding agents (harnesses).

Move with up/down, then enable or reconfigure the highlighted agent, disable it,
make it the project default, print its configuration (v), or edit one of its
files in your editor (f). Enabling runs the agent's interactive configure flow
in place, then returns to the menu.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			model := newConfigureModel(cmd.Context(), a, client, projectID)
			program := tea.NewProgram(model,
				tea.WithContext(cmd.Context()),
				tea.WithInput(cmd.InOrStdin()),
				tea.WithOutput(cmd.OutOrStdout()),
			)
			_, err = program.Run()
			return err
		},
	}
}

type configureModel struct {
	ctx       context.Context
	a         *App
	client    *apiclientgen.Client
	projectID string

	harnesses []apimodel.HarnessConfig
	defaultID string
	cursor    int

	status        string
	statusIsError bool
	busy          bool
	quitting      bool

	// confirmDisable holds the harness awaiting a disable confirmation, since
	// disabling deletes the agent's secrets and configuration.
	confirmDisable *apimodel.HarnessConfig

	// filePick, when set, replaces the agent list with the highlighted agent's
	// file list so the user can pick one to edit.
	filePick *configureFilePick
}

// configureFilePick is the file-selection submode entered with f: choose one
// of the agent's files, then hand the terminal to the editor.
type configureFilePick struct {
	cfg    apimodel.HarnessConfig
	files  []harnessFileRef
	cursor int
}

func newConfigureModel(ctx context.Context, a *App, client *apiclientgen.Client, projectID string) *configureModel {
	return &configureModel{ctx: ctx, a: a, client: client, projectID: projectID, status: "loading agents…", busy: true}
}

// configure TUI messages.
type (
	configureLoadedMsg struct {
		harnesses []apimodel.HarnessConfig
		defaultID string
		err       error
	}
	configureActionMsg struct {
		status string
		err    error
	}
	// configureViewMsg carries the rendered config card for the highlighted
	// agent, produced after fetching its secret bindings.
	configureViewMsg struct {
		detail string
		err    error
	}
)

func (m *configureModel) Init() tea.Cmd {
	return m.loadCmd()
}

func (m *configureModel) loadCmd() tea.Cmd {
	return func() tea.Msg {
		harnesses, err := m.a.listHarnessConfigs(m.ctx, m.client, m.projectID)
		if err != nil {
			return configureLoadedMsg{err: err}
		}
		defaultID, err := m.a.defaultHarnessConfigID(m.ctx, m.client, m.projectID)
		if err != nil {
			return configureLoadedMsg{err: err}
		}
		return configureLoadedMsg{harnesses: harnesses, defaultID: defaultID}
	}
}

func (m *configureModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case configureLoadedMsg:
		m.busy = false
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.harnesses = sortHarnesses(msg.harnesses)
		m.defaultID = msg.defaultID
		if m.cursor >= len(m.harnesses) {
			m.cursor = max(0, len(m.harnesses)-1)
		}
		if m.status == "loading agents…" {
			m.status = ""
		}
		return m, nil

	case configureActionMsg:
		m.busy = false
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.status = msg.status
		m.statusIsError = false
		return m, m.loadCmd()

	case configureViewMsg:
		m.busy = false
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.status = ""
		m.statusIsError = false
		return m, tea.Println(msg.detail)

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *configureModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.busy {
		// Ignore input while an action is in flight, except quit.
		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil
	}
	// File-pick mode owns the keys until the user edits a file or backs out.
	if m.filePick != nil {
		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "q", "esc":
			m.filePick = nil
			return m, nil
		case "up", "k":
			if m.filePick.cursor > 0 {
				m.filePick.cursor--
			}
		case "down", "j":
			if m.filePick.cursor < len(m.filePick.files)-1 {
				m.filePick.cursor++
			}
		case "enter", "e":
			return m, m.editPickedFile()
		}
		return m, nil
	}
	// A pending disable confirmation captures every key: y confirms, anything
	// else cancels, so an accidental keystroke never deletes an agent's config.
	if m.confirmDisable != nil {
		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "y", "Y":
			return m, m.runDisable(*m.confirmDisable)
		default:
			cfg := m.confirmDisable
			m.confirmDisable = nil
			m.status = fmt.Sprintf("kept %s", configureDisplayName(*cfg))
			m.statusIsError = false
			return m, nil
		}
	}
	switch msg.String() {
	case "q", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.harnesses)-1 {
			m.cursor++
		}
	case "enter", "e":
		return m, m.configureSelected()
	case "d", "x":
		m.beginDisable()
	case "s", "*":
		return m, m.setDefaultSelected()
	case "v", "i":
		return m, m.printSelectedConfig()
	case "f":
		m.beginFilePick()
	case "r":
		m.busy = true
		m.status = "refreshing…"
		return m, m.loadCmd()
	}
	return m, nil
}

func (m *configureModel) selected() (apimodel.HarnessConfig, bool) {
	if m.cursor < 0 || m.cursor >= len(m.harnesses) {
		return apimodel.HarnessConfig{}, false
	}
	return m.harnesses[m.cursor], true
}

// configureSelected hands the terminal to the agent's interactive configure
// flow via tea.Exec, which releases the terminal until the flow exits.
func (m *configureModel) configureSelected() tea.Cmd {
	cfg, ok := m.selected()
	if !ok {
		return nil
	}
	exec := &harnessConfigureExec{ctx: m.ctx, a: m.a, client: m.client, projectID: m.projectID, harnessID: cfg.ID}
	name := configureDisplayName(cfg)
	return tea.Exec(exec, func(err error) tea.Msg {
		if err != nil {
			return configureActionMsg{err: fmt.Errorf("configure %s: %w", name, err)}
		}
		return configureActionMsg{status: fmt.Sprintf("configured %s", name)}
	})
}

// beginDisable arms the disable confirmation. Disabling runs the harness's
// deconfigure flow, which deletes the secrets and files the configure flow
// created, so the user confirms before anything is removed.
func (m *configureModel) beginDisable() {
	cfg, ok := m.selected()
	if !ok {
		return
	}
	if !cfg.Configured {
		m.status = fmt.Sprintf("%s is not enabled", configureDisplayName(cfg))
		m.statusIsError = true
		return
	}
	captured := cfg
	m.confirmDisable = &captured
	m.status = ""
	m.statusIsError = false
}

func (m *configureModel) runDisable(cfg apimodel.HarnessConfig) tea.Cmd {
	m.confirmDisable = nil
	m.busy = true
	m.status = fmt.Sprintf("disabling %s…", configureDisplayName(cfg))
	name := configureDisplayName(cfg)
	// The server refuses to disable the project default, so release it first when
	// the target is the default. This is the automatic "switch/unset" the user
	// would otherwise have to do by hand.
	wasDefault := cfg.ID == m.defaultID
	return func() tea.Msg {
		if wasDefault {
			if _, err := m.client.UnsetDefaultHarnessConfig(m.ctx, apiclientgen.UnsetDefaultHarnessConfigParams{ProjectId: m.projectID, HarnessConfigId: cfg.ID}); err != nil {
				return configureActionMsg{err: err}
			}
		}
		res, err := m.client.DeconfigureHarnessConfig(m.ctx, apiclientgen.DeconfigureHarnessConfigParams{ProjectId: m.projectID, HarnessConfigId: cfg.ID})
		if err != nil {
			return configureActionMsg{err: err}
		}
		if _, err := expectResponse[apimodel.HarnessConfig](res); err != nil {
			return configureActionMsg{err: err}
		}
		return configureActionMsg{status: fmt.Sprintf("disabled %s", name)}
	}
}

func (m *configureModel) setDefaultSelected() tea.Cmd {
	cfg, ok := m.selected()
	if !ok {
		return nil
	}
	if !cfg.Configured {
		m.status = fmt.Sprintf("enable %s before making it the default", configureDisplayName(cfg))
		m.statusIsError = true
		return nil
	}
	m.busy = true
	m.status = fmt.Sprintf("setting %s as default…", configureDisplayName(cfg))
	name := configureDisplayName(cfg)
	return func() tea.Msg {
		if err := m.a.setDefaultHarnessConfig(m.ctx, m.client, m.projectID, cfg.ID); err != nil {
			return configureActionMsg{err: err}
		}
		return configureActionMsg{status: fmt.Sprintf("%s is now the default", name)}
	}
}

// printSelectedConfig prints a formatted card of the highlighted agent's
// configuration into the terminal scrollback above the menu, so it persists
// after the menu exits and can be copied. It first fetches the agent's secret
// bindings and the project's secrets so the card can show which secret is
// actually assigned to each declared environment variable.
func (m *configureModel) printSelectedConfig() tea.Cmd {
	cfg, ok := m.selected()
	if !ok {
		return nil
	}
	isDefault := cfg.ID == m.defaultID
	m.busy = true
	m.status = fmt.Sprintf("loading %s config…", configureDisplayName(cfg))
	m.statusIsError = false
	return func() tea.Msg {
		bindings, secretsByID, err := m.a.harnessSecretAssignments(m.ctx, m.client, m.projectID, cfg.ID)
		if err != nil {
			return configureViewMsg{err: err}
		}
		return configureViewMsg{detail: renderHarnessConfigDetail(cfg, isDefault, bindings, secretsByID)}
	}
}

// beginFilePick enters the file-selection submode for the highlighted agent.
func (m *configureModel) beginFilePick() {
	cfg, ok := m.selected()
	if !ok {
		return
	}
	files := harnessFileRefs(&cfg)
	if len(files) == 0 {
		m.status = fmt.Sprintf("%s has no files to edit", configureDisplayName(cfg))
		m.statusIsError = true
		return
	}
	m.filePick = &configureFilePick{cfg: cfg, files: files}
	m.status = ""
	m.statusIsError = false
}

// editPickedFile hands the terminal to the user's editor for the selected
// file, then saves the result back through the update API.
func (m *configureModel) editPickedFile() tea.Cmd {
	pick := m.filePick
	if pick == nil || pick.cursor < 0 || pick.cursor >= len(pick.files) {
		return nil
	}
	m.filePick = nil
	cfg := pick.cfg
	path := pick.files[pick.cursor].Path
	exec := &harnessEditExec{ctx: m.ctx, a: m.a, client: m.client, projectID: m.projectID, cfg: &cfg, path: path}
	return tea.Exec(exec, func(err error) tea.Msg {
		if err != nil {
			return configureActionMsg{err: fmt.Errorf("edit %s: %w", path, err)}
		}
		if !exec.changed {
			return configureActionMsg{status: fmt.Sprintf("%s unchanged", path)}
		}
		return configureActionMsg{status: fmt.Sprintf("updated %s", path)}
	})
}

func (m *configureModel) setError(err error) {
	m.status = err.Error()
	m.statusIsError = true
}

var (
	configureTitleStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	configureCursorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	configureDefaultStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	configureOnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	configureOffStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	configureHelpStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	configureErrStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	configureWarnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true)
	// configureErrBadgeStyle renders errors as a filled red bar so they read as
	// unmistakably wrong rather than as faint red text.
	configureErrBadgeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("15")).Background(lipgloss.Color("1")).Bold(true).Padding(0, 1)
)

func (m *configureModel) View() tea.View {
	if m.quitting {
		// Leave a single trailing newline so the prompt returns cleanly.
		return tea.NewView("")
	}
	var b strings.Builder
	if m.filePick != nil {
		fmt.Fprintf(&b, "%s\n\n", configureTitleStyle.Render("Files — "+configureDisplayName(m.filePick.cfg)))
		for i, file := range m.filePick.files {
			cursor := "  "
			if i == m.filePick.cursor {
				cursor = configureCursorStyle.Render("▸ ")
			}
			line := fmt.Sprintf("%s%-40s %s", cursor, file.Path, configureHelpStyle.Render(string(file.Bucket)))
			if i == m.filePick.cursor {
				line = lipgloss.NewStyle().Bold(true).Render(line)
			}
			fmt.Fprintln(&b, line)
		}
		b.WriteString("\n")
		message, actions := m.footerLines()
		fmt.Fprintln(&b, message)
		fmt.Fprintln(&b, actions)
		return tea.NewView(b.String())
	}
	fmt.Fprintf(&b, "%s\n\n", configureTitleStyle.Render("Coding agents"))

	if len(m.harnesses) == 0 && !m.busy {
		fmt.Fprintln(&b, configureOffStyle.Render("  No agents found. Register one with `disco box harnesses create`."))
	}
	for i, cfg := range m.harnesses {
		cursor := "  "
		if i == m.cursor {
			cursor = configureCursorStyle.Render("▸ ")
		}
		state := configureOffStyle.Render("disabled")
		if cfg.Configured {
			state = configureOnStyle.Render("enabled")
		} else if strings.TrimSpace(cfg.ConfigureError.Or("")) != "" {
			state = configureErrStyle.Render("failed")
		}
		def := "  "
		if cfg.ID == m.defaultID {
			def = configureDefaultStyle.Render("★ ")
		}
		name := configureDisplayName(cfg)
		line := fmt.Sprintf("%s%s%-20s %s", cursor, def, name, state)
		if i == m.cursor {
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		fmt.Fprintln(&b, line)
	}

	b.WriteString("\n")
	// The footer is always exactly two lines — a message line and an actions line
	// — so the layout never shifts when a status or confirmation appears.
	message, actions := m.footerLines()
	fmt.Fprintln(&b, message)
	fmt.Fprintln(&b, actions)

	return tea.NewView(b.String())
}

// footerLines returns the two fixed footer rows: the message row (blank, a
// status, an error bar, or a disable warning) and the actions row (the key hints
// or the confirm prompt).
func (m *configureModel) footerLines() (message, actions string) {
	if m.filePick != nil {
		actions = configureHelpStyle.Render("↑/↓ move · enter edit · esc back")
		switch {
		case m.status == "":
			message = ""
		case m.statusIsError:
			message = configureErrBadgeStyle.Render("✗ " + m.status)
		default:
			message = configureHelpStyle.Render(m.status)
		}
		return message, actions
	}
	if m.confirmDisable != nil {
		name := configureDisplayName(*m.confirmDisable)
		warning := fmt.Sprintf("Disabling %s deletes its secrets and configuration.", name)
		if m.confirmDisable.ID == m.defaultID {
			warning = fmt.Sprintf("%s It is the project default and will be unset first.", warning)
		}
		return configureWarnStyle.Render(warning), configureHelpStyle.Render("Continue? y to disable · any other key to cancel")
	}

	actions = configureHelpStyle.Render("↑/↓ move · e enable/reconfigure · d disable · s set default · v view config · f edit files · r refresh · q/esc quit")
	switch {
	case m.status == "":
		message = "" // reserved blank line, keeps the height constant
	case m.statusIsError:
		message = configureErrBadgeStyle.Render("✗ " + m.status)
	default:
		message = configureHelpStyle.Render(m.status)
	}
	return message, actions
}

// harnessConfigureExec adapts runHarnessConfigure to tea.ExecCommand so the
// inline menu can release the terminal and hand it to the harness's interactive
// configure flow, resuming when it exits.
type harnessConfigureExec struct {
	ctx       context.Context
	a         *App
	client    *apiclientgen.Client
	projectID string
	harnessID string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
}

func (e *harnessConfigureExec) SetStdin(r io.Reader)  { e.stdin = r }
func (e *harnessConfigureExec) SetStdout(w io.Writer) { e.stdout = w }
func (e *harnessConfigureExec) SetStderr(w io.Writer) { e.stderr = w }

func (e *harnessConfigureExec) Run() error {
	_, err := e.a.runHarnessConfigure(e.ctx, e.client, e.projectID, e.harnessID, e.stdin, e.stdout, e.stderr)
	return err
}

// harnessEditExec adapts editHarnessFile to tea.ExecCommand, so the menu can
// hand the terminal to the user's editor and resume when it exits. changed
// records whether the edit was saved, for the completion status.
type harnessEditExec struct {
	ctx       context.Context
	a         *App
	client    *apiclientgen.Client
	projectID string
	cfg       *apimodel.HarnessConfig
	path      string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	changed   bool
}

func (e *harnessEditExec) SetStdin(r io.Reader)  { e.stdin = r }
func (e *harnessEditExec) SetStdout(w io.Writer) { e.stdout = w }
func (e *harnessEditExec) SetStderr(w io.Writer) { e.stderr = w }

func (e *harnessEditExec) Run() error {
	changed, err := e.a.editHarnessFile(e.ctx, e.client, e.projectID, e.cfg, e.path, e.stdin, e.stdout, e.stderr)
	e.changed = changed
	return err
}

var (
	configureDetailLabelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	configureDetailHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
)

// renderHarnessConfigDetail renders an agent's full configuration as a
// human-readable card: identity, state, image, commands, declared secrets with
// the secret actually assigned to each (ID and type), and the files the image
// and the configure flow contribute, including their contents.
func renderHarnessConfigDetail(cfg apimodel.HarnessConfig, isDefault bool, bindings []apimodel.HarnessConfigSecretBinding, secretsByID map[string]apimodel.Secret) string {
	label := func(s string) string { return configureDetailLabelStyle.Render(fmt.Sprintf("  %-12s", s)) }

	state := "disabled"
	switch {
	case cfg.Configured:
		state = configureOnStyle.Render("enabled")
	case strings.TrimSpace(cfg.ConfigureError.Or("")) != "":
		state = configureErrStyle.Render("failed")
	default:
		state = configureOffStyle.Render(state)
	}

	var b strings.Builder
	header := configureDetailHeaderStyle.Render(configureDisplayName(cfg))
	if cfg.Slug != "" && cfg.Slug != configureDisplayName(cfg) {
		header += configureHelpStyle.Render(" (" + cfg.Slug + ")")
	}
	fmt.Fprintln(&b, header)
	fmt.Fprintln(&b, label("ID"), cfg.ID)
	status := state
	if isDefault {
		status += configureDefaultStyle.Render(" ★ default")
	}
	if cfg.BuiltIn {
		status += configureHelpStyle.Render(" · built-in")
	}
	fmt.Fprintln(&b, label("State"), status)
	if reason := strings.TrimSpace(cfg.ConfigureError.Or("")); reason != "" {
		fmt.Fprintln(&b, label("Error"), configureErrStyle.Render(reason))
	}
	if image := cfg.Image.Or(""); image != "" {
		fmt.Fprintln(&b, label("Image"), image)
	}
	if digest := cfg.ImageDigest.Or(""); digest != "" {
		fmt.Fprintln(&b, label("Digest"), digest)
	}
	if len(cfg.RunCommand) > 0 {
		fmt.Fprintln(&b, label("Run"), strings.Join(cfg.RunCommand, " "))
	}
	if relaunch := cfg.RelaunchCommand.Or(nil); len(relaunch) > 0 {
		fmt.Fprintln(&b, label("Relaunch"), strings.Join(relaunch, " "))
	}
	boundByEnv := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		boundByEnv[binding.EnvName] = binding.SecretId
	}
	secretsLabel := "Secrets"
	writeSecretRow := func(line string) {
		fmt.Fprintln(&b, label(secretsLabel), line)
		secretsLabel = ""
	}
	for _, secret := range cfg.Secrets.Or(nil) {
		var notes []string
		if secret.Required.Or(false) {
			notes = append(notes, "required")
		}
		if group := secret.OneOfGroup.Or(""); group != "" {
			notes = append(notes, "one of "+group)
		}
		line := secret.Name
		if len(notes) > 0 {
			line += configureHelpStyle.Render(" (" + strings.Join(notes, ", ") + ")")
		}
		if secretID, ok := boundByEnv[secret.Name]; ok {
			line += " → " + describeAssignedSecret(secretID, secretsByID)
			delete(boundByEnv, secret.Name)
		} else {
			line += configureOffStyle.Render(" → not bound")
		}
		writeSecretRow(line)
	}
	// Bindings for env vars the image did not declare (e.g. custom secrets the
	// user bound by hand) still count as assigned; show them too.
	for _, binding := range bindings {
		secretID, ok := boundByEnv[binding.EnvName]
		if !ok {
			continue
		}
		writeSecretRow(binding.EnvName + configureHelpStyle.Render(" (undeclared)") + " → " + describeAssignedSecret(secretID, secretsByID))
	}
	writeFiles := func(name string, files []apimodel.HarnessConfigFile) {
		for i, file := range files {
			if i > 0 {
				name = ""
			}
			var notes []string
			notes = append(notes, formatBytes(int64(len(file.Content))))
			if file.CreateOnly.Or(false) {
				notes = append(notes, "create-only")
			}
			if file.Template.Or(false) {
				notes = append(notes, "template")
			}
			fmt.Fprintln(&b, label(name), file.Path+configureHelpStyle.Render(" ("+strings.Join(notes, ", ")+")"))
			if file.Content != "" {
				for _, contentLine := range strings.Split(strings.TrimRight(file.Content, "\n"), "\n") {
					fmt.Fprintln(&b, "               "+contentLine)
				}
			}
		}
	}
	writeFiles("Files", cfg.Files.Or(nil))
	writeFiles("Configured", cfg.ConfiguredFiles.Or(nil))
	fmt.Fprintln(&b, label("Updated"), formatTime(cfg.UpdatedAt))
	return strings.TrimRight(b.String(), "\n")
}

// describeAssignedSecret renders a bound secret as its ID plus the secret's
// type and name when the secret is visible in the project's secret list.
func describeAssignedSecret(secretID string, secretsByID map[string]apimodel.Secret) string {
	secret, ok := secretsByID[secretID]
	if !ok {
		return secretID
	}
	notes := []string{string(secret.Type)}
	if name := strings.TrimSpace(secret.Name); name != "" && name != secretID {
		notes = append(notes, name)
	}
	if secret.Anonymous.Or(false) {
		notes = append(notes, "anonymous")
	}
	return secretID + configureHelpStyle.Render(" ("+strings.Join(notes, ", ")+")")
}

func configureDisplayName(cfg apimodel.HarnessConfig) string {
	if name := strings.TrimSpace(cfg.Name); name != "" {
		return name
	}
	if slug := strings.TrimSpace(cfg.Slug); slug != "" {
		return slug
	}
	return cfg.ID
}

func sortHarnesses(harnesses []apimodel.HarnessConfig) []apimodel.HarnessConfig {
	sorted := make([]apimodel.HarnessConfig, len(harnesses))
	copy(sorted, harnesses)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
	})
	return sorted
}
