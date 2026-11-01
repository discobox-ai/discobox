// Package builtin is how one of the image's built-in monitors (ADR
// 26-10-31-073 §9) is written in Go rather than as a script (ADR
// 26-11-01-477 §1). Its declaration is still a script in the image's
// directory, one that execs the agent's `monitor-builtin NAME` subcommand.
// So it keeps a declaration's name, options and interval, and its instance's
// runner, state directory, failure events and queue, and the framework has
// no second kind of monitor.
//
// A built-in is a Monitor: one run of it, given the instance's Env. It prints
// events through Env.Emit, keeps what it needs between runs in Env.State, and
// fails by returning an error, whose text is the failure the runner reports.
package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/discobox-ai/discobox/sandbox-agent/monitors"
)

// Command is the agent's subcommand a built-in's declared script execs, with
// the built-in's name after it:
//
//	exec discobox-sandbox-agent monitor-builtin github-repo
const Command = "monitor-builtin"

// Env is one run's instance, as the runner hands it to the monitor's process.
type Env struct {
	// Instance is the instance's name (DISCOBOX_MONITOR_NAME).
	Instance string
	// State is the monitor's own directory (DISCOBOX_MONITOR_STATE), empty at
	// an instance's first run.
	State string
	// Options are the instance's options (DISCOBOX_MONITOR_OPTIONS).
	Options map[string]string
	// Getenv reads the rest of the monitor's environment: its credentials,
	// its proxy.
	Getenv func(string) string
	// Emit prints one event. Only the fields a monitor may print are kept
	// (monitors.ParseLine); the runner adds the rest.
	Emit func(monitors.Event) error
	// Stderr is the instance's log.
	Stderr io.Writer
}

// Monitor is one run of a built-in.
type Monitor func(ctx context.Context, env Env) error

// Main runs the built-in args names, from set, with the environment the
// runner gave it, and is the process's exit status.
func Main(ctx context.Context, set map[string]Monitor, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "usage: discobox-sandbox-agent %s NAME\n", Command)
		return 2
	}
	run, ok := set[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "%s: no built-in monitor %q\n", Command, args[0])
		return 2
	}
	env, err := FromEnviron(getenv, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s %s: %v\n", Command, args[0], err)
		return 2
	}
	if err := run(ctx, env); err != nil {
		if ctx.Err() != nil {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// FromEnviron is the Env the runner's variables describe, emitting to stdout
// a JSON line per event.
func FromEnviron(getenv func(string) string, stdout, stderr io.Writer) (Env, error) {
	env := Env{
		Instance: getenv(monitors.EnvName),
		State:    getenv(monitors.EnvState),
		Getenv:   getenv,
		Emit:     LineEmitter(stdout),
		Stderr:   stderr,
	}
	if env.State == "" {
		return Env{}, errors.New(monitors.EnvState + " is not set: a built-in runs as a monitor, under discobox-monitor")
	}
	env.Options = map[string]string{}
	if raw := strings.TrimSpace(getenv(monitors.EnvOptions)); raw != "" {
		if err := json.Unmarshal([]byte(raw), &env.Options); err != nil {
			return Env{}, fmt.Errorf("%s: %w", monitors.EnvOptions, err)
		}
	}
	return env, nil
}

// LineEmitter prints each event as the one JSON line the runner reads.
func LineEmitter(w io.Writer) func(monitors.Event) error {
	var mu sync.Mutex
	return func(ev monitors.Event) error {
		data, err := monitors.FormatLine(ev)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		_, err = w.Write(append(data, '\n'))
		return err
	}
}

// ReadState reads the JSON file name in the state directory into v, and
// reports false, leaving v alone, when there is none yet.
func (e Env) ReadState(name string, v any) (bool, error) {
	data, err := os.ReadFile(filepath.Join(e.State, name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return true, nil
}

// WriteState writes v whole as the JSON file name in the state directory:
// under a dot name, then renamed over it, so a run ended part way leaves the
// last whole state rather than half of a new one.
func (e Env) WriteState(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.State, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(e.State, "."+name+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(e.State, name)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
