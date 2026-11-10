package console

import (
	"context"
	"io"
	"strings"
	"testing"
)

// A transcript longer than a pane could usefully hold is cut, at a line
// boundary so the cut never lands inside an escape sequence.
func TestHistoryIsTailedAtALineBoundary(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("x", 99) + "\n"
	logs := []byte(strings.Repeat(line, (historyLimit/len(line))+50))
	got := tailHistory(logs)
	if len(got) > historyLimit {
		t.Fatalf("history is %d bytes, want at most %d", len(got), historyLimit)
	}
	if !strings.HasPrefix(string(got), "x") {
		t.Fatalf("history starts mid-line: %q", string(got[:20]))
	}
	if short := []byte("short\n"); len(tailHistory(short)) != len(short) {
		t.Fatal("a transcript under the limit must be played whole")
	}
}

// Every verb is offered whatever the service is doing; only a declaration
// that cannot run refuses them, saying why.
func TestServiceActions(t *testing.T) {
	t.Parallel()
	for _, s := range []Service{{ID: "web", Status: "running"}, {ID: "web", Status: "stopped"}} {
		for _, a := range s.Actions() {
			if !a.Enabled || a.Why != "" {
				t.Errorf("%s on a %s service: enabled %v, why %q", a.Verb, s.Status, a.Enabled, a.Why)
			}
		}
	}
	broken := Service{ID: "web", Status: "stopped", Problem: "it has no shebang"}
	got := broken.Actions()
	if len(got) != 3 || got[0].Verb != ServiceStart || got[1].Verb != ServiceStop || got[2].Verb != ServiceRestart {
		t.Fatalf("actions = %+v, want start, stop, restart", got)
	}
	for _, a := range got {
		if a.Enabled || a.Why != broken.Problem {
			t.Errorf("%s on a service that cannot run: enabled %v, why %q", a.Verb, a.Enabled, a.Why)
		}
	}
}

// The tabs a service listing opens are the services worth one, keyed by the
// service and placed by their declaration order: one stopped on purpose has
// none.
func TestListedServiceTabs(t *testing.T) {
	t.Parallel()
	services := []Service{
		{ID: "web", Status: "running", ExecID: "exec_1"},
		{ID: "worker", Status: "stopped"},
		{ID: "migrate", Status: "exited"},
		{ID: "broken", Status: "stopped", Problem: "not executable"},
	}
	got := ListedServiceTabs(services)
	want := []struct {
		id     string
		order  int
		status string
	}{{"web", 0, ""}, {"migrate", 2, "exited"}, {"broken", 3, "cannot run"}}
	if len(got) != len(want) {
		t.Fatalf("tabs = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		if got[i].ExecID != ServiceTabID(w.id) || got[i].ServiceOrder != w.order || got[i].Status != w.status {
			t.Errorf("tab %d = %+v, want %s at %d saying %q", i, got[i], w.id, w.order, w.status)
		}
	}
}

// A service that is not running is opened on its card, its last output under
// it; one that is, on its process, after what it has written so far.
func TestOpenService(t *testing.T) {
	t.Parallel()
	code := 1
	ds := &serviceSource{logs: "listening on :3000\n", live: "request served\r\n"}
	term, err := OpenService(context.Background(), ds, "sbx", Service{ID: "web", Name: "Web", Status: "failed", ExitCode: &code}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	card := readSome(t, term)
	for _, want := range []string{"Web\r\n", "failed", "exit code 1", "── last output ──", "listening on :3000"} {
		if !strings.Contains(card, want) {
			t.Errorf("the card does not say %q:\n%s", want, card)
		}
	}
	if ds.opened != "" {
		t.Errorf("a service not running was attached to %q", ds.opened)
	}
	_ = term.Close()

	term, err = OpenService(context.Background(), ds, "sbx", Service{ID: "web", Status: "running", ExecID: "exec_web"}, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	if got := readSome(t, term) + readSome(t, term); got != "listening on :3000\nrequest served\r\n" {
		t.Errorf("a running service reads %q, want its history then its stream", got)
	}
	if ds.opened != "exec_web" {
		t.Errorf("attached to %q, want the service's exec", ds.opened)
	}
}

func readSome(t *testing.T, r io.Reader) string {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

// serviceSource is a DataSource with one service's transcript, and a live
// stream that says one thing.
type serviceSource struct {
	DataSource
	logs, live string
	opened     string
	services   []Service
	done       []string
}

func (s *serviceSource) ServiceLogs(context.Context, string, string) ([]byte, error) {
	return []byte(s.logs), nil
}

func (s *serviceSource) OpenExec(_ context.Context, _, execID string, _, _ int) (Terminal, error) {
	s.opened = execID
	return newTextTerminal([]byte(s.live)), nil
}

// A verb is run as the service's menu offers it, on the service read again:
// one that cannot run, one no longer declared and a verb a service has not
// are refused unsent, in words to say as they are; one that ran says so.
func TestRunServiceVerb(t *testing.T) {
	t.Parallel()
	ds := &serviceSource{services: []Service{{ID: "web", Name: "Web", Status: "stopped"}, {ID: "broken", Status: "stopped", Problem: "it has no shebang"}}}
	ctx := context.Background()
	if note, err := RunServiceVerb(ctx, ds, "sbx", "web", ServiceStart); err != nil || note != "started Web" {
		t.Fatalf("start: %q, %v", note, err)
	}
	for _, c := range []struct {
		service string
		verb    ServiceVerb
		want    string
	}{
		{"broken", ServiceStart, "cannot start broken: it has no shebang"},
		{"gone", ServiceStop, `this discobox declares no service "gone"`},
		{"web", ServiceVerb("pause"), `a service has no verb "pause"`},
	} {
		if _, err := RunServiceVerb(ctx, ds, "sbx", c.service, c.verb); err == nil || err.Error() != c.want {
			t.Errorf("%s %s: %v, want %q", c.verb, c.service, err, c.want)
		}
	}
	if want := []string{"start sbx web"}; strings.Join(ds.done, ",") != strings.Join(want, ",") {
		t.Errorf("sent %q, want %q", ds.done, want)
	}
}

func (s *serviceSource) Services(context.Context, string) ([]Service, error) { return s.services, nil }

func (s *serviceSource) DoService(_ context.Context, verb ServiceVerb, sandboxID, serviceID string) error {
	s.done = append(s.done, string(verb)+" "+sandboxID+" "+serviceID)
	return nil
}
