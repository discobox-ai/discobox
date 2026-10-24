package cli

import (
	"context"
	"errors"
	"fmt"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/tools"
)

// newToolsCommand groups the commands that run a development tool against a
// sandbox, with the tool's own arguments passed through untouched.
//
// git and ssh are commands of their own. Every other tool is a declaration
// (ADR 0125): the CLI's, the discobox's image's or primary source's, or the
// user's own, and `discobox tools <id>` runs whichever one wins. The tools this
// machine declares are subcommands, so help and completion know them; a tool
// only the discobox declares cannot be known until one is chosen, and is run
// by the same command built on demand.
func (a *App) newToolsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tools TOOL [DISCOBOX] [-- ARG...]",
		Aliases: []string{"tool", "t"},
		Short:   "Run a development tool against a discobox",
		Long: `Run a development tool against a discobox.

DISCOBOX is the NAME "discobox ls" prints, an ID, or a short prefix of one.
Without it the discobox is taken from the ones "discobox ls" shows: the only one
when there is one, otherwise you are asked to pick. The tool's own arguments go
after --.

git runs inside the discobox and ssh opens a session to it. Every other tool is
declared in a file, and "discobox tools ls" lists the ones a discobox offers.
A tool runs either in the discobox or here, handed the discobox's ssh host,
working tree or git URL. Tools are declared, the last one winning on a shared
name, by:

  this CLI                            vscode, zed
  the discobox's image                /usr/local/share/discobox/tools
  the discobox's primary source       .discobox/tools
  you                                 ` + userToolsDirHelp() + `

A declaration is a .yaml file naming the program it runs, or a script with a
front-matter block that is itself what runs. Only this CLI and you can declare
a tool that runs on this machine.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			// A tool this machine does not declare: build its command now and
			// run it on the arguments the lookup did not consume.
			tool := a.newToolRunCommand(args[0], "")
			tool.SetContext(cmd.Context())
			tool.SetArgs(args[1:])
			tool.SetIn(cmd.InOrStdin())
			tool.SetOut(cmd.OutOrStdout())
			tool.SetErr(cmd.ErrOrStderr())
			tool.SilenceUsage, tool.SilenceErrors = true, true
			return tool.Execute()
		},
	}
	// Flags stop at the tool's name, so everything after it is the tool's
	// command's to parse.
	cmd.Flags().SetInterspersed(false)

	cmd.AddCommand(a.newToolsGitCommand())
	cmd.AddCommand(a.newToolsSSHCommand())
	cmd.AddCommand(a.newToolsListCommand())
	// A user directory that cannot be read leaves its tools to the on-demand
	// path, which reports why when one is asked for.
	local, _ := localTools()
	for _, def := range local {
		if reservedToolIDs[def.ID] {
			continue
		}
		cmd.AddCommand(a.newToolRunCommand(def.ID, def.Description))
	}
	return cmd
}

// userToolsDirHelp is the user's tools directory as help names it.
func userToolsDirHelp() string {
	dir, err := toolConfigDir()
	if err != nil {
		return "<config dir>/discobox/tools"
	}
	return dir
}

// newToolRunCommand is `discobox tools <id>`, for any declared tool.
func (a *App) newToolRunCommand(id, short string) *cobra.Command {
	var source, program string
	if short == "" {
		short = "Run the " + id + " tool"
	}
	// A tool's description is a fragment the console shows beside its key, and
	// a repository writes it however it likes; help reads it as a sentence.
	if r, size := utf8.DecodeRuneInString(short); r != utf8.RuneError {
		short = string(unicode.ToUpper(r)) + short[size:]
	}
	cmd := &cobra.Command{
		Use:   id + " [DISCOBOX] [flags] [-- ARG...]",
		Short: short,
		Long: short + `.

DISCOBOX is the NAME "discobox ls" prints, an ID, or a short prefix of one;
without it the only discobox there is, or a pick. Everything after -- is
passed to the tool untouched.`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runTool(cmd, id, source, program, args)
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "Source to work in, named by its slug; defaults to the discobox's primary source")
	cmd.Flags().StringVar(&program, "program", "", "Program a tool that runs here runs, instead of the first of its declared programs on PATH")
	return cmd
}

// runTool runs one declared tool against the discobox the command names.
func (a *App) runTool(cmd *cobra.Command, id, source, program string, args []string) error {
	target, rest, err := a.resolveToolTarget(cmd, source, args)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	// A catalog the discobox posted before it stopped is good enough to find
	// the tool by: running it starts the discobox, which runs what it
	// declares now.
	catalog, _, err := target.app.toolCatalog(ctx, target.client, target.projectID, target.sandboxID)
	if errors.Is(err, errSandboxToolsNotReadable) {
		// Reading the tools does not start a discobox, and this one is down
		// with none posted. Running a tool is use, though, and use starts it:
		// run nothing in it, which waits for it to come up, then ask it.
		if _, _, _, startErr := target.app.sandboxCommandOutput(ctx, target.projectID, target.sandboxID, "", []string{"true"}); startErr != nil {
			return fmt.Errorf("%w; starting the discobox to read them: %w", err, startErr)
		}
		catalog, _, err = target.app.toolCatalog(ctx, target.client, target.projectID, target.sandboxID)
	}
	if err != nil {
		return err
	}
	def, err := findTool(catalog, id)
	if err != nil {
		return err
	}
	if def.Runs == tools.RunsHost {
		// The user typed this command, so what it does on their behalf is
		// printed where its own reporting goes.
		return target.app.runHostTool(ctx, def, target, program, rest, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), printedNotes(cmd.ErrOrStderr()))
	}
	if program != "" {
		return fmt.Errorf("tool %s runs in the discobox, and --program names the program a tool that runs here runs", id)
	}
	// Before the session, not alongside it: the tool reads its configuration
	// when it starts, so a file that lands a moment later is a file this run
	// never saw.
	if err := target.app.installToolFiles(ctx, target.projectID, target.sandboxID, def); err != nil {
		return err
	}
	command, err := target.app.sandboxToolCommand(ctx, target.projectID, target.sandboxID, def, rest)
	if err != nil {
		return err
	}
	return target.app.runToolInSelected(cmd, target.projectID, target.sandboxID, target.client, source, def.Env, command)
}

// newToolsListCommand is `discobox tools ls`: every tool a discobox offers,
// where each was declared, and why any of them cannot run.
func (a *App) newToolsListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:               "ls [DISCOBOX]",
		Aliases:           []string{"list"},
		Short:             "List the tools a discobox can be worked on with",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, _, err := a.resolveToolTarget(cmd, "", args)
			if err != nil {
				return err
			}
			catalog, observedAt, err := target.app.toolCatalog(cmd.Context(), target.client, target.projectID, target.sandboxID)
			if err != nil {
				return err
			}
			defer noteStoredAnswer(cmd, observedAt)
			if a.quiet {
				return writeResourceIDs(cmd.OutOrStdout(), catalog, func(def tools.Definition) string { return def.ID })
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "TOOL\tNAME\tKEY\tRUNS\tFROM\tDESCRIPTION")
			for _, def := range catalog {
				about := def.Description
				if def.Problem != "" {
					about = "cannot run: " + def.Problem
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", def.ID, def.Name, def.Key, def.Runs, def.Layer, about)
			}
			return w.Flush()
		},
	}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newToolsGitCommand() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use:   "git [DISCOBOX] [flags] -- ARG...",
		Short: "Run git in a discobox source's working tree",
		Long: `Run git inside a discobox, in the working tree of one of its sources.

DISCOBOX is the NAME "discobox ls" prints, an ID, or a short prefix of one;
without it the only discobox there is, or a pick. Without --source git runs in
the discobox's default working directory, which is its primary source.

Everything after -- is passed to git as is.`,
		Example: `  discobox tools git -- status
  discobox tools git mybox --source docs -- log --oneline -5
  discobox tools git sbx_01hq -- diff`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			app, projectID, sandboxID, client, gitArgs, err := a.selectDiscoboxCommand(cmd, args)
			if err != nil {
				return err
			}
			if len(gitArgs) == 0 {
				return fmt.Errorf("git's arguments go after --: %s -- status", cmd.CommandPath())
			}
			return app.runToolInSelected(cmd, projectID, sandboxID, client, source, nil, append([]string{"git"}, gitArgs...))
		},
	}
	cmd.Flags().StringVar(&source, "source", "", "Source to run in, named by its slug; defaults to the discobox's primary source")
	return cmd
}

// runToolInSelected runs command inside the discobox, in the working tree of
// the selected source, streamed to this terminal exactly like `discobox
// shell`, by the App aimed at the server it is on (selectSandbox).
func (a *App) runToolInSelected(cmd *cobra.Command, projectID, sandboxID string, client *apiclientgen.Client, sourceSlug string, env, command []string) error {
	ctx := cmd.Context()
	var err error
	// The primary source needs no workdir and therefore no sandbox record: an
	// exec with no workdir already lands in the sandbox's default exec
	// directory, which is the primary source's. Naming another source with
	// --source is the only thing that has to look one up, so the common
	// `discobox t git status` is create + attach and nothing else.
	workdir := ""
	if sourceSlug != "" {
		workdir, err = a.toolSourceWorkdir(ctx, client, projectID, sandboxID, sourceSlug)
		if err != nil {
			return err
		}
	}

	tty := isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.OutOrStdout()) && isTerminalStream(cmd.ErrOrStderr())
	body, err := createSandboxExecBody(sandboxExecCreateOptions{interactive: true, tty: tty, workdir: workdir, env: env}, command)
	if err != nil {
		return err
	}
	exec, err := a.createSandboxExec(ctx, projectID, sandboxID, body, true)
	if err != nil {
		return err
	}
	if err := a.attachSandboxExec(ctx, projectID, sandboxID, exec.ID, true, tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		return err
	}
	return a.returnSandboxExecStatus(ctx, projectID, sandboxID, exec.ID)
}

// toolSourceWorkdir is the directory the source named by slug lives at inside
// the sandbox, from the sandbox record.
func (a *App) toolSourceWorkdir(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, slug string) (string, error) {
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return "", err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return "", err
	}
	return sourceSlugWorkdir(sandbox, slug)
}

// sourceSlugWorkdir is the directory the source named by slug lives at inside
// sandbox.
func sourceSlugWorkdir(sandbox *apimodel.Sandbox, slug string) (string, error) {
	for _, entry := range applySources(sandbox) {
		if entry.slug != slug {
			continue
		}
		workdir := sourceWorkdir(entry.source)
		if workdir == "" {
			return "", fmt.Errorf("source %s has no known directory in the discobox", slug)
		}
		return workdir, nil
	}
	return "", fmt.Errorf("discobox has no source with slug %q", slug)
}
