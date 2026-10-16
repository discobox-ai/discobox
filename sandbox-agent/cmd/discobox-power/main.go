// Command discobox-power is how an agent in a discobox holds it up, puts it to
// sleep, and names when to wake it (ADR 0108 §4, ADR 26-10-16-337 §3).
//
// It only writes files — a keepalive lease, a sleep request, a wake — each put
// in place with its time already set, and reads them back. The files are the
// interface: `touch` and `mv` do everything this does, and nothing reads state
// only this writes.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/autostop"
)

const usage = `usage:
  discobox-power sleep [--wake-in DUR | --wake-at TIME] [--note TEXT]
  discobox-power wake NAME (--in DUR | --at TIME) [--note TEXT]
  discobox-power wake ls
  discobox-power wake rm NAME
  discobox-power keepalive NAME (--for DUR | --until TIME)
  discobox-power keepalive rm NAME
  discobox-power status

TIME is RFC 3339 (2026-10-16T14:00:00Z) or a clock time (14:00), the next one
to come in local time. DUR is a Go duration (45m, 2h).`

// dirs is where each kind of file lives, and the marker that says the control
// plane holds the sandbox's power; tests point them elsewhere.
type dirs struct {
	lease, sleep, wake, controlPlane string
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "discobox-power:", err)
		os.Exit(1)
	}
	d := dirs{
		lease:        autostop.DefaultLeaseDir,
		sleep:        autostop.DefaultSleepDir,
		wake:         filepath.Join(home, ".discobox", "wake"),
		controlPlane: autostop.DefaultControlPlaneMarker,
	}
	if err := run(d, os.Args[1:], time.Now(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "discobox-power:", err)
		var u usageError
		if errors.As(err, &u) {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func run(d dirs, args []string, now time.Time, out io.Writer) error {
	if len(args) == 0 {
		return usageError{"no command"}
	}
	switch args[0] {
	case "sleep":
		return sleep(d, args[1:], now, out)
	case "wake":
		return wake(d, args[1:], now, out)
	case "keepalive":
		return keepalive(d, args[1:], now, out)
	case "status":
		return status(d, now, out)
	case "-h", "--help", "help":
		fmt.Fprintln(out, usage)
		return nil
	}
	return usageError{fmt.Sprintf("unknown command %q", args[0])}
}

// sleep writes the wake first, when there is one, so the status that carries
// the sleep carries the wake too.
func sleep(d dirs, args []string, now time.Time, out io.Writer) error {
	fs := flag.NewFlagSet("sleep", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	in := fs.String("wake-in", "", "wake after this long")
	at := fs.String("wake-at", "", "wake at this time")
	note := fs.String("note", "", "note for the harness on waking")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError{"sleep takes only flags"}
	}
	if *in != "" || *at != "" {
		when, err := pickTime(*in, *at, now)
		if err != nil {
			return err
		}
		if err := put(d.wake, "sleep", *note, when); err != nil {
			return err
		}
		fmt.Fprintf(out, "wake at %s\n", when.Local().Format(time.RFC3339))
		warnUnwoken(d, out)
	} else if *note != "" {
		return usageError{"--note needs --wake-in or --wake-at"}
	}
	if err := put(d.sleep, "agent", "", now); err != nil {
		return err
	}
	fmt.Fprintln(out, "sleep requested; the sandbox powers down within a minute")
	return nil
}

func wake(d dirs, args []string, now time.Time, out io.Writer) error {
	if len(args) == 0 {
		return usageError{"wake needs a NAME, ls, or rm"}
	}
	switch args[0] {
	case "ls":
		for _, r := range sortedByTime(autostop.ReadRequests(d.wake)) {
			fmt.Fprintf(out, "%s\t%s\n", r.At.Local().Format(time.RFC3339), r.Name)
		}
		return nil
	case "rm":
		if len(args) != 2 {
			return usageError{"wake rm takes one NAME"}
		}
		return remove(d.wake, args[1])
	}
	name := args[0]
	fs := flag.NewFlagSet("wake", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	in := fs.String("in", "", "wake after this long")
	at := fs.String("at", "", "wake at this time")
	note := fs.String("note", "", "note for the harness on waking")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usageError{"wake NAME takes only flags"}
	}
	when, err := pickTime(*in, *at, now)
	if err != nil {
		return err
	}
	if err := put(d.wake, name, *note, when); err != nil {
		return err
	}
	fmt.Fprintf(out, "wake %s at %s\n", name, when.Local().Format(time.RFC3339))
	warnUnwoken(d, out)
	return nil
}

// warnUnwoken says so when nothing will act on a wake: the pool has not handed
// the sandbox's power to the control plane, so a stopped sandbox stays
// stopped until someone uses it (ADR 26-10-16-337 §8).
func warnUnwoken(d dirs, out io.Writer) {
	if _, err := os.Stat(d.controlPlane); err == nil {
		return
	}
	fmt.Fprintln(out, "warning: this sandbox's pool does not wake sandboxes yet; once it stops, nothing starts it at that time")
}

func keepalive(d dirs, args []string, now time.Time, out io.Writer) error {
	if len(args) == 0 {
		return usageError{"keepalive needs a NAME or rm"}
	}
	if args[0] == "rm" {
		if len(args) != 2 {
			return usageError{"keepalive rm takes one NAME"}
		}
		return remove(d.lease, args[1])
	}
	name := args[0]
	fs := flag.NewFlagSet("keepalive", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	forDur := fs.String("for", "", "hold the sandbox up this long")
	until := fs.String("until", "", "hold the sandbox up until this time")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usageError{"keepalive NAME takes only flags"}
	}
	when, err := pickTime(*forDur, *until, now)
	if err != nil {
		return err
	}
	if err := put(d.lease, name, "", when); err != nil {
		return err
	}
	fmt.Fprintf(out, "kept alive until %s\n", when.Local().Format(time.RFC3339))
	return nil
}

// status reads the files back. The idle policy's own view — when the sandbox
// stops if nothing happens — is on the sandbox's status, which the console
// shows.
func status(d dirs, now time.Time, out io.Writer) error {
	for _, section := range []struct {
		title    string
		requests []autostop.Request
	}{{"keepalive", autostop.ReadLeases(d.lease)}, {"sleep", autostop.ReadRequests(d.sleep)}, {"wake", autostop.ReadRequests(d.wake)}} {
		requests := sortedByTime(section.requests)
		if len(requests) == 0 {
			fmt.Fprintf(out, "%s: none\n", section.title)
			continue
		}
		fmt.Fprintf(out, "%s:\n", section.title)
		for _, r := range requests {
			when := "past"
			if r.At.After(now) {
				when = "in " + r.At.Sub(now).Round(time.Second).String()
			}
			fmt.Fprintf(out, "  %s\t%s\t%s\n", r.Name, r.At.Local().Format(time.RFC3339), when)
		}
	}
	return nil
}

// pickTime takes exactly one of a duration from now or a time.
func pickTime(in, at string, now time.Time) (time.Time, error) {
	switch {
	case in != "" && at != "":
		return time.Time{}, usageError{"give a duration or a time, not both"}
	case in != "":
		d, err := time.ParseDuration(in)
		if err != nil || d <= 0 {
			return time.Time{}, usageError{fmt.Sprintf("%q is not a positive duration", in)}
		}
		return now.Add(d), nil
	case at != "":
		return parseTime(at, now)
	}
	return time.Time{}, usageError{"give a duration or a time"}
}

// parseTime reads RFC 3339, or a clock time taken as the next one to come.
func parseTime(text string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, text); err == nil {
		return t, nil
	}
	clock, err := time.ParseInLocation("15:04", text, now.Location())
	if err != nil {
		return time.Time{}, usageError{fmt.Sprintf("%q is neither RFC 3339 nor HH:MM", text)}
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, nil
}

// put writes name in dir dated when. The file is written and dated under a
// name beginning with ".", which nothing reads, and only then renamed into
// place: a wake written first and dated after would be due the moment it was
// written (ADR 26-10-16-337 §2).
func put(dir, name, note string, when time.Time) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".discobox-power-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if note != "" {
		if _, err := tmp.WriteString(note + "\n"); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Chtimes(tmp.Name(), when, when); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

func remove(dir, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// checkName refuses a name that is not one file in the directory, or that
// nothing would read.
func checkName(name string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) {
		return usageError{fmt.Sprintf("%q is not a name: one path segment, not beginning with '.'", name)}
	}
	return nil
}

// sortedByTime orders requests by their time, earliest first.
func sortedByTime(requests []autostop.Request) []autostop.Request {
	sort.Slice(requests, func(i, j int) bool { return requests[i].At.Before(requests[j].At) })
	return requests
}
