package sandboxcreate

import "time"

// StallClock bounds a wait by silence rather than by total elapsed time.
//
// A total budget has to be guessed against the slowest thing the wait can
// contain, and for anything waiting on a discobox that is an image pull over
// whatever connection the user has. Two minutes was such a guess, and a 1.4 GiB
// harness image failed against it with the byte counter still climbing on
// screen: nothing about the wait was wrong except the clock it was measured by.
//
// What separates a wait worth continuing from one worth abandoning is whether
// anything is still happening, and a discobox says so — every phase is recorded
// as it is entered, and a pull restates its byte counts twice a second. So the
// deadline moves whenever the reported status does, and only silence spends it.
type StallClock struct {
	window   time.Duration
	deadline time.Time
}

// NewStallClock starts a clock that expires after window without progress.
func NewStallClock(window time.Duration) *StallClock {
	return &StallClock{window: window, deadline: time.Now().Add(window)}
}

// Progressed restarts the clock. Callers call it when the thing they are
// watching said something new.
func (c *StallClock) Progressed() {
	if c == nil {
		return
	}
	c.deadline = time.Now().Add(c.window)
}

// Expired reports whether nothing has happened for the whole window.
func (c *StallClock) Expired() bool {
	return c != nil && time.Now().After(c.deadline)
}

// Window is how long the silence has to last, for a caller writing the message
// that says so.
func (c *StallClock) Window() time.Duration {
	if c == nil {
		return 0
	}
	return c.window
}
