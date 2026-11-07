package console

import (
	"fmt"
	"strings"
	"time"
)

// Service is one of a discobox's declared services, as the menu draws it.
type Service struct {
	ID          string
	Name        string
	Description string
	// Status is the sandbox's own word for what the service is doing:
	// stopped, starting, running, exited or failed.
	Status string
	// Problem is why a declaration cannot run — a missing shebang or
	// executable bit. Such a service is listed, because the alternative is a
	// file the author believes is a service and that nothing ever mentions.
	Problem string

	// FileName is the declaring file, which is what a pane points at when it
	// has to say where to go and fix something.
	FileName string
	// ExecID is the exec running, or last run, for this service. Empty when
	// none ever has.
	ExecID string
	// StartedAt is when the current or last run began. With ExecID it says
	// whether a pane is looking at the run the server is reporting: a restart
	// keeps the exec id (ADR 0038) and moves this.
	StartedAt time.Time
	// ExitCode is how the last run ended, absent when it has not ended.
	ExitCode *int
	// Error is what the last run failed with, when it did.
	Error string
}

// Runnable reports whether this declaration can be acted on at all.
func (s Service) Runnable() bool { return strings.TrimSpace(s.Problem) == "" }

// Detail is the row's second line: why it cannot run, or what it is for.
func (s Service) Detail() string {
	if !s.Runnable() {
		return "cannot run: " + s.Problem
	}
	if description := strings.TrimSpace(s.Description); description != "" {
		return s.Status + " · " + description
	}
	return s.Status
}

// DisplayName is what the service is called on screen.
func (s Service) DisplayName() string {
	if name := strings.TrimSpace(s.Name); name != "" {
		return name
	}
	return s.ID
}

// PaneWorthy reports whether a service should have a tab.
//
// Running is obvious. The rest is the answer to "would its absence be a
// surprise": a service that failed, one that ended on its own, and a
// declaration that cannot run are all things you would otherwise learn about
// only by pressing a key you had no reason to press. A service you stopped
// yourself is the one case where absence says the right thing — you know, and a
// tab you have to dismiss every time would be the window nagging.
func (s Service) PaneWorthy() bool {
	switch {
	case !s.Runnable():
		return true
	case s.Live():
		return true
	case s.Status == "failed", s.Status == "exited":
		return true
	default:
		return false
	}
}

// Live reports whether there is a running process to attach to.
func (s Service) Live() bool { return s.Status == "running" || s.Status == "starting" }

// RunKey identifies the run a pane is looking at, so the workspace can notice
// when it is looking at a stale one. A restart keeps the exec id and moves the
// start time; a stop clears the liveness; a fixed declaration clears the
// problem. Any of those means the pane has to be opened again.
func (s Service) RunKey() string {
	return fmt.Sprintf("%s|%s|%s|%s", s.ExecID, s.StartedAt.UTC().Format(time.RFC3339Nano), s.Status, s.Problem)
}

// TabMark is the sign the tab strip wears beside a service that is not running:
// enough to tell "it is fine" from "go and look" without opening it.
func (s Service) TabMark() string {
	switch {
	case !s.Runnable(), s.Status == "failed":
		return " ✗"
	case s.Status == "exited":
		return " ·"
	default:
		return ""
	}
}

// PaneStatus is the word the pane's own header wears for a service that is not
// running, so the state is readable without reading the card.
func (s Service) PaneStatus() string {
	if !s.Runnable() {
		return "cannot run"
	}
	return s.Status
}
