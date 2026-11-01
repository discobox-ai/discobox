package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors/builtin"
	"github.com/discobox-ai/discobox/sandbox-agent/monitors/githubrepo"
)

const monitorUsage = `usage:
  discobox-monitor list [--json]
  discobox-monitor show MONITOR [--json]
  discobox-monitor enable MONITOR [--name NAME] [--update] [-o KEY=VALUE]...
  discobox-monitor disable NAME
  discobox-monitor status [NAME] [--json]
  discobox-monitor logs NAME [--lines N]
  discobox-monitor pop [--wait[=DUR]] [--follow] [--name NAME] [--max N] [--json]
  discobox-monitor peek [--name NAME] [--max N] [--json]
  discobox-monitor read ID [--json]
  discobox-monitor emit --title TEXT [--kind KIND] [--key KEY] [--summary TEXT]
                        [--url URL] [--body-file FILE|-] [--name NAME]
  discobox-monitor test MONITOR [-o KEY=VALUE]... [--for DUR] [--since DUR]

Enabling a monitor watches; only a running 'pop --wait' wakes you. Events wait
until you pop them, and 'read ID' shows one whole. A monitor is a script in the
repository's .discobox/monitors or the image's, named by its id, or anywhere,
named by its path (a skill's own: enable ./monitors/watch.sh). 'test' runs one
without enabling it, and 'test --since 2h' starts it two hours back, for a
monitor that can.

'enable --update' gives an enabled instance new options in place, keeping what
it already watched; the options given replace its options whole.

-o takes KEY=VALUE, repeated or joined by commas (-o repo=foo/bar,pr=412).
DUR is a Go duration (30m, 2h).`

// monitorEnv is where discobox-monitor reads and writes; tests point it
// elsewhere.
type monitorEnv struct {
	dirs       monitors.Dirs
	home       string
	builtinDir string
	// sourceRoot is the primary source's directory, where the repository's
	// monitors are declared; empty outside a sandbox that has one.
	sourceRoot string
	now        func() time.Time
	ctx        context.Context
}

// runMonitor is discobox-monitor (ADR 26-10-31-073 §1). Like discobox-power it
// only reads and writes files — the enabled instances, the queue — that the
// agent's reconciler and the instances' runners read and write.
func runMonitor(args []string) int {
	if !monitors.Supported {
		fmt.Fprintln(os.Stderr, "discobox-monitor: monitors are not available on a Windows sandbox yet")
		return 1
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-monitor:", err)
		return 1
	}
	// The repository's monitors are where the agent's own discovery is
	// rooted: the primary source, as the agent's config derives it from the
	// bootstrap. A bootstrap that cannot be read is said, not taken for a
	// repository that declares nothing.
	var sourceRoot string
	if cfg, err := config.Load(config.DefaultPath); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-monitor: the repository's monitors are unavailable:", safe(err.Error(), true))
	} else {
		sourceRoot = cfg.ExecDefaults.SourceRoot
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := monitorEnv{
		dirs:       monitors.HomeDirs(home),
		home:       home,
		builtinDir: monitors.BuiltinDir,
		sourceRoot: sourceRoot,
		now:        time.Now,
		ctx:        ctx,
	}
	if err := monitor(env, args, os.Stdout); err != nil {
		// Escaped here, whatever the error: one can carry a path or a name a
		// third party chose, such as a skills source's skill directory.
		fmt.Fprintln(os.Stderr, "discobox-monitor:", safe(err.Error(), true))
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintln(os.Stderr, monitorUsage)
			return 2
		}
		return 1
	}
	return 0
}

// runMonitorRunner is the `monitor-run NAME` subcommand an instance's exec
// runs (monitors.Runner). An instance no longer enabled exits at once, which
// is what a runner relaunched after a disable should do.
func runMonitorRunner(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: discobox-sandbox-agent monitor-run NAME")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "monitor-run:", err)
		return 1
	}
	dirs := monitors.HomeDirs(home)
	inst, err := monitors.ReadInstance(dirs, args[0])
	if err != nil {
		if errors.Is(err, monitors.ErrNotEnabled) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "monitor-run:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner := &monitors.Runner{Dirs: dirs, Instance: inst}
	if err := runner.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "monitor-run:", err)
		return 1
	}
	return 0
}

// builtinMonitors are the monitors written in Go, each run by its declared
// script through `monitor-builtin NAME`.
var builtinMonitors = map[string]builtin.Monitor{
	githubrepo.Name: githubrepo.Run,
}

// runMonitorBuiltin is the `monitor-builtin NAME` subcommand: one run of a
// built-in, under the runner, as its script would be.
func runMonitorBuiltin(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return builtin.Main(ctx, builtinMonitors, args, os.Getenv, os.Stdout, os.Stderr)
}

func monitor(env monitorEnv, args []string, out io.Writer) error {
	if len(args) == 0 {
		return usageError{"no command"}
	}
	if env.ctx == nil {
		env.ctx = context.Background()
	}
	switch args[0] {
	case "list", "ls":
		return monitorList(env, args[1:], out)
	case "show":
		return monitorShow(env, args[1:], out)
	case "enable":
		return monitorEnable(env, args[1:], out)
	case "disable":
		return monitorDisable(env, args[1:], out)
	case "status":
		return monitorStatus(env, args[1:], out)
	case "logs":
		return monitorLogs(env, args[1:], out)
	case "pop":
		return monitorPop(env, args[1:], out, true)
	case "peek":
		return monitorPop(env, args[1:], out, false)
	case "read":
		return monitorRead(env, args[1:], out)
	case "emit":
		return monitorEmit(env, args[1:], out)
	case "test":
		return monitorTest(env, args[1:], out)
	case "help", "-h", "--help":
		fmt.Fprintln(out, monitorUsage)
		return nil
	}
	return usageError{fmt.Sprintf("unknown command %q", args[0])}
}

// parseInterspersed parses flags wherever they sit among the arguments, and
// returns the positional ones: `enable github-repo -o repo=a/b` reads as
// naturally as the flags first.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// waitFlag is --wait or --wait=DUR: bare, it waits for as long as it takes.
type waitFlag struct {
	set bool
	dur time.Duration
}

func (w *waitFlag) String() string   { return "" }
func (w *waitFlag) IsBoolFlag() bool { return true }
func (w *waitFlag) Set(v string) error {
	switch v {
	case "true":
		w.set, w.dur = true, 0
		return nil
	case "false":
		w.set = false
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return fmt.Errorf("--wait=%s is not a duration (30m)", v)
	}
	w.set, w.dur = true, d
	return nil
}

func (env monitorEnv) discover() ([]monitors.Definition, error) {
	return monitors.Discover(env.builtinDir, env.sourceRoot)
}

// definition is the monitor an argument names: by its path when it has a `/`
// (or the platform's own separator) in it, wherever the file is — a skill's
// own monitor, say (ADR 26-11-01-375 §1) — and otherwise by its id in the
// declaration directories. A path is
// made absolute against the working directory and cleaned, with its links
// kept: a skill linked in from a checkout runs the checkout's file.
func (env monitorEnv) definition(id string) (monitors.Definition, error) {
	if strings.ContainsRune(id, '/') || strings.ContainsRune(id, filepath.Separator) {
		path, err := filepath.Abs(id)
		if err != nil {
			return monitors.Definition{}, err
		}
		if info, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return monitors.Definition{}, fmt.Errorf("monitor %s: no such file", path)
		} else if err != nil {
			return monitors.Definition{}, fmt.Errorf("monitor %s: %w", path, err)
		} else if info.IsDir() {
			return monitors.Definition{}, fmt.Errorf("monitor %s is a directory; name the script in it", path)
		}
		return monitors.Load(path, filepath.Dir(path) == filepath.Clean(env.builtinDir))
	}
	defs, err := env.discover()
	if err != nil {
		return monitors.Definition{}, err
	}
	def, ok := monitors.Find(defs, id)
	if !ok {
		ids := make([]string, 0, len(defs))
		for _, d := range defs {
			ids = append(ids, oneLine(d.ID))
		}
		known := "none are declared"
		if len(ids) > 0 {
			known = "there are " + strings.Join(ids, ", ")
		}
		return monitors.Definition{}, fmt.Errorf("no monitor %q: %s (discobox-monitor list, or name a script by its path)", id, known)
	}
	return def, nil
}

func monitorList(env monitorEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageError{"list takes no arguments"}
	}
	defs, err := env.discover()
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSONLines(out, defs)
	}
	if len(defs) == 0 {
		fmt.Fprintf(out, "no monitors declared; write one in %s\n", filepath.Join(env.sourceRootOr("<repository>"), monitors.DirName))
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MONITOR\tFROM\tRUNS\tOPTIONS\tDESCRIPTION")
	for _, def := range defs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", oneLine(def.ID), env.source(def), mode(def, nil), optionSummary(def), oneLine(def.Description))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, def := range defs {
		if def.Problem != "" {
			fmt.Fprintln(out, cannotRun(def))
		}
	}
	return nil
}

// workdir is where every monitor runs, the image's included: the
// repository, when the box has one, so a relative path or a `make` in an
// option means what it would in the agent's own shell (ADR 26-10-31-073 §3).
func (env monitorEnv) workdir() string {
	if env.sourceRoot != "" {
		return env.sourceRoot
	}
	return env.home
}

func (env monitorEnv) sourceRootOr(fallback string) string {
	if env.sourceRoot != "" {
		return env.sourceRoot
	}
	return fallback
}

// cannotRun says why a declaration cannot run. A declaration is third-party
// text — a skills source's author's, a repository's — so all of it is
// escaped.
func cannotRun(def monitors.Definition) string {
	return fmt.Sprintf("%s cannot run: %s (%s)", oneLine(def.ID), oneLine(def.Problem), oneLine(def.Path))
}

func (env monitorEnv) source(def monitors.Definition) string {
	switch {
	case def.Builtin:
		return "image"
	case env.sourceRoot != "" && filepath.Dir(def.Path) == filepath.Join(env.sourceRoot, monitors.DirName):
		return "repo"
	}
	return "path"
}

// mode is how the monitor runs: for an instance given opts, or with nil, as
// declared, saying when an instance may choose its own interval.
func mode(def monitors.Definition, opts map[string]string) string {
	if def.Interval == 0 {
		return "streaming"
	}
	if opts != nil {
		if interval, err := def.IntervalFor(opts); err == nil {
			return "every " + interval.String()
		}
	}
	if _, ok := def.Option(monitors.IntervalOption); ok {
		return fmt.Sprintf("every %s (-o interval=, at least %s)", def.Interval, max(def.MinInterval, monitors.MinInterval))
	}
	return "every " + def.Interval.String()
}

func optionSummary(def monitors.Definition) string {
	if len(def.Options) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(def.Options))
	for _, opt := range def.Options {
		name := opt.Name
		if opt.Required {
			name += "*"
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, ",")
}

func monitorShow(env monitorEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageError{"show takes one monitor"}
	}
	def, err := env.definition(rest[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSONLines(out, []monitors.Definition{def})
	}
	fmt.Fprintf(out, "%s — %s\n", oneLine(def.ID), oneLine(def.Name))
	if def.Description != "" {
		fmt.Fprintf(out, "  %s\n", oneLine(def.Description))
	}
	fmt.Fprintf(out, "  from:  %s (%s)\n", env.source(def), oneLine(def.Path))
	fmt.Fprintf(out, "  runs:  %s\n", mode(def, nil))
	if def.Since {
		fmt.Fprintln(out, "  since: test --since starts it from a baseline in the past")
	}
	if def.Problem != "" {
		fmt.Fprintf(out, "  cannot run: %s\n", oneLine(def.Problem))
	}
	if len(def.Options) > 0 {
		fmt.Fprintln(out, "  options:")
		for _, opt := range def.Options {
			required := ""
			if opt.Required {
				required = " (required)"
			}
			fmt.Fprintf(out, "    %s%s  %s\n", opt.Name, required, oneLine(opt.Description))
		}
	}
	instances, _ := monitors.Enabled(env.dirs)
	for _, inst := range instances {
		if inst.Monitor == def.ID {
			fmt.Fprintf(out, "  enabled as %s: %s\n", inst.Name, oneLine(inst.Describe()))
		}
	}
	return nil
}

func monitorEnable(env monitorEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("enable", flag.ContinueOnError)
	name := fs.String("name", "", "")
	update := fs.Bool("update", false, "")
	var opts stringList
	fs.Var(&opts, "o", "")
	fs.Var(&opts, "option", "")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageError{"enable takes one monitor"}
	}
	def, err := env.definition(rest[0])
	if err != nil {
		return err
	}
	if def.Problem != "" {
		return errors.New(cannotRun(def))
	}
	options, err := monitors.ParseOptions(opts)
	if err != nil {
		return usageError{err.Error()}
	}
	if err := monitors.CheckOptions(def, options); err != nil {
		return err
	}
	instName := *name
	if instName == "" {
		instName = def.ID
	}
	workdir := env.workdir()
	inst := monitors.Instance{
		Name:      instName,
		Monitor:   def.ID,
		Path:      def.Path,
		Builtin:   def.Builtin,
		Workdir:   workdir,
		Options:   options,
		EnabledAt: env.now().UTC(),
	}
	if len(inst.Options) == 0 {
		inst.Options = nil
	}
	result, err := monitors.Enable(env.dirs, inst, *update)
	if err != nil {
		if errors.Is(err, monitors.ErrNameTaken) {
			// It names the instance's monitor, a declaration's id.
			if *update {
				return errors.New(oneLine(err.Error()) + "; --update changes an instance's options, not its monitor: pass --name to enable another")
			}
			return errors.New(oneLine(err.Error()) + "; pass --update to change its options, or --name to enable another")
		}
		return err
	}
	switch result {
	case monitors.Unchanged:
		fmt.Fprintf(out, "%s is already enabled: %s\n", inst.Name, oneLine(inst.Describe()))
		return nil
	case monitors.Updated:
		fmt.Fprintf(out, "updated %s: %s; it keeps what it already watched\n", inst.Name, oneLine(inst.Describe()))
		return nil
	}
	fmt.Fprintf(out, "enabled %s: %s, %s\n", inst.Name, oneLine(inst.Describe()), mode(def, options))
	fmt.Fprintln(out, "Its events wait for 'discobox-monitor pop'. To be woken by one, keep 'discobox-monitor pop --wait' running.")
	return nil
}

func monitorDisable(env monitorEnv, args []string, out io.Writer) error {
	if len(args) != 1 {
		return usageError{"disable takes one instance name"}
	}
	if err := monitors.Disable(env.dirs, args[0]); err != nil {
		if errors.Is(err, monitors.ErrNotEnabled) {
			return fmt.Errorf("%w (discobox-monitor status)", err)
		}
		return err
	}
	fmt.Fprintf(out, "disabled %s\n", args[0])
	if n, err := env.dirs.Queue().Count(monitors.Filter{Instance: args[0]}); err == nil && n > 0 {
		fmt.Fprintf(out, "%d of its events are still pending: discobox-monitor pop --name %s\n", n, args[0])
	}
	return nil
}

// instanceStatus is one row of `status`.
type instanceStatus struct {
	Name    string            `json:"name"`
	Monitor string            `json:"monitor"`
	Options map[string]string `json:"options,omitempty"`
	Enabled bool              `json:"enabled"`
	Pending int               `json:"pending"`
	monitors.Status
}

func monitorStatus(env monitorEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usageError{"status takes at most one instance name"}
	}
	instances, problems := monitors.Enabled(env.dirs)
	var rows []instanceStatus
	queue := env.dirs.Queue()
	for _, inst := range instances {
		row := instanceStatus{Name: inst.Name, Monitor: inst.Monitor, Options: inst.Options, Enabled: true}
		st, ok, err := monitors.ReadStatus(env.dirs, inst.Name)
		if err != nil {
			return err
		}
		if !ok {
			st = monitors.Status{Instance: inst.Name, Monitor: inst.Monitor, State: monitors.StateStarting}
		}
		row.Status = st
		rows = append(rows, row)
	}
	finished, err := monitors.Finished(env.dirs)
	if err != nil {
		return err
	}
	for _, st := range finished {
		rows = append(rows, instanceStatus{Name: st.Instance, Monitor: st.Monitor, Status: st})
	}
	if len(rest) == 1 {
		rows = slicesFilter(rows, func(r instanceStatus) bool { return r.Name == rest[0] })
		if len(rows) == 0 {
			return fmt.Errorf("%w: %q (discobox-monitor status)", monitors.ErrNotEnabled, rest[0])
		}
	}
	for i := range rows {
		rows[i].Pending, _ = queue.Count(monitors.Filter{Instance: rows[i].Name})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	total, _ := queue.Count(monitors.Filter{})
	if *asJSON {
		return writeJSONLines(out, rows)
	}
	for _, problem := range problems {
		fmt.Fprintln(out, "unreadable:", problem)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no monitors enabled (discobox-monitor list, then enable)")
	} else {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tMONITOR\tSTATE\tRUNS\tEVENTS\tPENDING\tLAST EVENT\tOPTIONS")
		for _, row := range rows {
			state := string(row.State)
			if row.Failing && row.State != monitors.StateFailed {
				state += " (failing)"
			}
			last := "-"
			if row.LastEventAt != nil {
				last = age(env.now(), *row.LastEventAt)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n", row.Name, row.Monitor, state, row.Runs, row.Events, row.Pending, last, optionsText(row.Options))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, row := range rows {
			if row.LastError != "" && (row.Failing || row.State == monitors.StateFailed) {
				fmt.Fprintf(out, "%s: %s (discobox-monitor logs %s)\n", row.Name, safe(row.LastError, false), row.Name)
			}
			if row.State == monitors.StateDone {
				fmt.Fprintf(out, "%s is done; discobox-monitor disable %s clears it\n", row.Name, row.Name)
			}
		}
	}
	fmt.Fprintf(out, "%d events pending\n", total)
	return nil
}

func slicesFilter[T any](in []T, keep func(T) bool) []T {
	var out []T
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

func optionsText(opts map[string]string) string {
	if len(opts) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+opts[k])
	}
	return strings.Join(parts, ",")
}

func monitorLogs(env monitorEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	lines := fs.Int("lines", 100, "")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageError{"logs takes one instance name"}
	}
	var text []byte
	for _, path := range monitors.LogPaths(env.dirs, rest[0]) {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		text = append(text, data...)
	}
	if len(text) == 0 {
		if _, err := os.Stat(env.dirs.InstanceStateDir(rest[0])); err != nil {
			return fmt.Errorf("%w: %q (discobox-monitor status)", monitors.ErrNotEnabled, rest[0])
		}
		fmt.Fprintf(out, "%s has written nothing to stderr\n", rest[0])
		return nil
	}
	all := strings.Split(strings.TrimRight(string(text), "\n"), "\n")
	if *lines > 0 && len(all) > *lines {
		all = all[len(all)-*lines:]
	}
	_, err = fmt.Fprintln(out, safe(strings.Join(all, "\n"), true))
	return err
}

func monitorPop(env monitorEnv, args []string, out io.Writer, take bool) error {
	verb := "peek"
	if take {
		verb = "pop"
	}
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	name := fs.String("name", "", "")
	limit := fs.Int("max", 0, "")
	asJSON := fs.Bool("json", false, "")
	var wait waitFlag
	follow := false
	if take {
		fs.Var(&wait, "wait", "")
		fs.BoolVar(&follow, "follow", false, "")
	}
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageError{verb + " takes no arguments"}
	}
	queue := env.dirs.Queue()
	filter := monitors.Filter{Instance: *name}
	if follow {
		// Each batch as it arrives, until interrupted: for a harness tool that
		// streams a command's lines.
		for {
			if err := queue.Wait(env.ctx, filter); err != nil {
				if env.ctx.Err() != nil {
					return nil
				}
				return err
			}
			events, err := queue.Pop(filter, *limit)
			if err != nil {
				return err
			}
			if err := printHeaders(env, out, events, *asJSON); err != nil {
				return err
			}
			if f, ok := out.(interface{ Sync() error }); ok {
				_ = f.Sync()
			}
		}
	}
	ctx := env.ctx
	if wait.set && wait.dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait.dur)
		defer cancel()
	}
	var events []monitors.Event
	for {
		if wait.set {
			if err := queue.Wait(ctx, filter); err != nil && ctx.Err() == nil {
				return err
			}
			if env.ctx.Err() != nil {
				return env.ctx.Err()
			}
		}
		if take {
			events, err = queue.Pop(filter, *limit)
		} else {
			events, err = queue.Peek(filter, *limit)
		}
		if err != nil {
			return err
		}
		// A wait that saw an event another pop then took has woken nobody:
		// it waits again, until it takes one or its time is up.
		if len(events) > 0 || !wait.set || ctx.Err() != nil {
			break
		}
	}
	if err := printHeaders(env, out, events, *asJSON); err != nil {
		return err
	}
	if *asJSON {
		return nil
	}
	left, _ := queue.Count(filter)
	switch {
	case len(events) == 0 && wait.set && wait.dur > 0:
		fmt.Fprintf(out, "no events in %s; %d pending\n", wait.dur, left)
	case len(events) == 0:
		fmt.Fprintln(out, "no events pending")
	case take:
		fmt.Fprintf(out, "%d popped, %d pending. 'discobox-monitor read ID' shows one whole.\n", len(events), left)
	default:
		fmt.Fprintf(out, "%d pending, none taken. 'discobox-monitor read ID' shows one whole.\n", left)
	}
	return nil
}

// header is what pop says of an event: everything but its body and data.
type header struct {
	ID            string    `json:"id"`
	Time          time.Time `json:"time"`
	Instance      string    `json:"instance"`
	Monitor       string    `json:"monitor,omitempty"`
	Kind          string    `json:"kind,omitempty"`
	Title         string    `json:"title"`
	Summary       string    `json:"summary,omitempty"`
	URL           string    `json:"url,omitempty"`
	Count         int       `json:"count,omitempty"`
	BodyBytes     int       `json:"bodyBytes,omitempty"`
	HasData       bool      `json:"hasData,omitempty"`
	BodyTruncated bool      `json:"bodyTruncated,omitempty"`
}

func printHeaders(env monitorEnv, out io.Writer, events []monitors.Event, asJSON bool) error {
	if asJSON {
		heads := make([]header, 0, len(events))
		for _, ev := range events {
			heads = append(heads, header{
				ID: ev.ID, Time: ev.Time, Instance: ev.Instance, Monitor: ev.Monitor, Kind: ev.Kind,
				Title: ev.Title, Summary: ev.Summary, URL: ev.URL, Count: ev.Count,
				BodyBytes: len(ev.Body), HasData: len(ev.Data) > 0, BodyTruncated: ev.BodyTruncated,
			})
		}
		return writeJSONLines(out, heads)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, ev := range events {
		kind := ev.Kind
		if kind == "" {
			kind = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ev.ID, age(env.now(), ev.Time), oneLine(ev.Instance), oneLine(kind), headline(ev))
	}
	return tw.Flush()
}

// headline is an event's one line: its title, its summary, and what else
// `read` has for it.
func headline(ev monitors.Event) string {
	text := oneLine(ev.Title)
	if ev.Summary != "" {
		text += " — " + oneLine(ev.Summary)
	}
	text = clip(text, 200)
	var extra []string
	if ev.Count > 1 {
		extra = append(extra, fmt.Sprintf("x%d", ev.Count))
	}
	if ev.URL != "" {
		extra = append(extra, oneLine(ev.URL))
	}
	if len(ev.Body) > 0 {
		extra = append(extra, "body "+size(len(ev.Body)))
	}
	if len(ev.Data) > 0 {
		extra = append(extra, "data")
	}
	if len(extra) > 0 {
		text += "  [" + strings.Join(extra, ", ") + "]"
	}
	return text
}

func oneLine(s string) string { return safe(strings.Join(strings.Fields(s), " "), false) }

// safe makes text a third party wrote — an issue's title, a page's body, a
// monitor's stderr — harmless to print to a terminal: every control character
// but newline and tab (those only when keepLines) is shown as an escape rather
// than sent, so no escape sequence in it reaches the terminal or, through it,
// the console. --json needs none of this; encoding/json escapes them.
func safe(s string, keepLines bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case keepLines && (r == '\n' || r == '\t'):
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func clip(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

func size(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func age(now, at time.Time) string {
	d := now.Sub(at)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

func monitorRead(env monitorEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageError{"read takes one event id"}
	}
	ev, err := env.dirs.Queue().Get(rest[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSONLines(out, []monitors.Event{ev})
	}
	fmt.Fprintf(out, "%s  %s (%s)\n", ev.ID, ev.Time.Local().Format(time.RFC3339), age(env.now(), ev.Time))
	if ev.Monitor != "" {
		fmt.Fprintf(out, "instance: %s (%s)\n", safe(ev.Instance, false), safe(ev.Monitor, false))
	} else {
		// Queued by `emit`, which no monitor ran.
		fmt.Fprintf(out, "instance: %s\n", safe(ev.Instance, false))
	}
	if ev.Kind != "" {
		fmt.Fprintf(out, "kind:     %s\n", safe(ev.Kind, false))
	}
	fmt.Fprintf(out, "title:    %s\n", safe(ev.Title, false))
	if ev.Summary != "" {
		fmt.Fprintf(out, "summary:  %s\n", safe(ev.Summary, false))
	}
	if ev.URL != "" {
		fmt.Fprintf(out, "url:      %s\n", safe(ev.URL, false))
	}
	if ev.Count > 1 {
		fmt.Fprintf(out, "count:    %d (newer events with key %q replaced older ones)\n", ev.Count, ev.Key)
	}
	if len(ev.Data) > 0 {
		fmt.Fprintf(out, "data:     %s\n", ev.Data)
	}
	if ev.Body != "" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, safe(strings.TrimRight(ev.Body, "\n"), true))
		if ev.BodyTruncated {
			fmt.Fprintf(out, "[body cut at %s]\n", size(monitors.MaxBody))
		}
	}
	return nil
}

func monitorEmit(env monitorEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("emit", flag.ContinueOnError)
	title := fs.String("title", "", "")
	kind := fs.String("kind", "", "")
	key := fs.String("key", "", "")
	summary := fs.String("summary", "", "")
	url := fs.String("url", "", "")
	bodyFile := fs.String("body-file", "", "")
	name := fs.String("name", "emit", "")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return usageError{"emit takes no arguments; the title is --title"}
	}
	if strings.TrimSpace(*title) == "" {
		return usageError{"emit needs --title"}
	}
	if err := monitors.ValidName(*name); err != nil {
		return usageError{err.Error()}
	}
	ev := monitors.Event{Instance: *name, Kind: *kind, Key: *key, Title: strings.TrimSpace(*title), Summary: *summary, URL: *url}
	if *bodyFile != "" {
		var body []byte
		if *bodyFile == "-" {
			body, err = io.ReadAll(io.LimitReader(os.Stdin, monitors.MaxBody+1))
		} else {
			body, err = os.ReadFile(*bodyFile)
		}
		if err != nil {
			return err
		}
		ev.Body = string(body)
		if len(ev.Body) > monitors.MaxBody {
			ev.Body, ev.BodyTruncated = ev.Body[:monitors.MaxBody], true
		}
	}
	queued, err := env.dirs.Queue().Add(ev)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "queued %s\n", queued.ID)
	return nil
}

func monitorTest(env monitorEnv, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var opts stringList
	fs.Var(&opts, "o", "")
	fs.Var(&opts, "option", "")
	runFor := fs.Duration("for", 0, "")
	since := fs.Duration("since", 0, "")
	rest, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return usageError{"test takes one monitor"}
	}
	def, err := env.definition(rest[0])
	if err != nil {
		return err
	}
	if def.Problem != "" {
		return errors.New(cannotRun(def))
	}
	if *since < 0 {
		return usageError{"--since takes a duration back from now (2h)"}
	}
	if *since > 0 && !def.Since {
		return fmt.Errorf("%s cannot start in the past: its front matter does not declare `since: true` (%s)", oneLine(def.ID), oneLine(def.Path))
	}
	options, err := monitors.ParseOptions(opts)
	if err != nil {
		return usageError{err.Error()}
	}
	if err := monitors.CheckOptions(def, options); err != nil {
		return err
	}
	state, err := os.MkdirTemp("", "discobox-monitor-test-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	workdir := env.workdir()
	ctx := env.ctx
	if *runFor > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *runFor)
		defer cancel()
	}
	var from time.Time
	baseline := "a fresh state directory"
	if *since > 0 {
		from = env.now().Add(-*since)
		baseline = "a baseline " + since.String() + " back"
	}
	fmt.Fprintf(os.Stderr, "testing %s (%s) from %s; nothing is enabled or queued. Ctrl-C ends it.\n", oneLine(def.ID), mode(def, options), baseline)
	count := 0
	runner := &monitors.Runner{
		Test:         true,
		Instance:     monitors.Instance{Name: "test", Monitor: def.ID, Path: def.Path, Builtin: def.Builtin, Workdir: workdir, Options: options},
		MonitorState: state,
		Since:        from,
		Stderr:       os.Stderr,
		Emit: func(ev monitors.Event) error {
			count++
			ev.ID = "test" + strconv.Itoa(count)
			ev.Time = env.now().UTC()
			return writeJSONLines(out, []monitors.Event{ev})
		},
	}
	if err := runner.Run(ctx); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%d events\n", count)
	return nil
}

func writeJSONLines[T any](out io.Writer, items []T) error {
	enc := json.NewEncoder(out)
	for _, item := range items {
		if err := enc.Encode(item); err != nil {
			return err
		}
	}
	return nil
}
