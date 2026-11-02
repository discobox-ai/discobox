package discoboxbox

import (
	"maps"
	"slices"
	"time"
)

// state is what one run leaves the next.
type state struct {
	// Baseline is when the first run began, on the box's clock. Zero before
	// it. A box whose first event was recorded before it was already there.
	Baseline time.Time `json:"baseline"`
	// Cursor is the last event ID the stream gave that is safe to resume
	// from: an event's, or one the stream sent alone. Empty before the first
	// run, whose stream starts with the project's current state.
	Cursor string `json:"cursor,omitempty"`
	// Boxes are every box in the project as its last event left it, watched
	// or not, by ID: a box is selected by what its events carry, so one not
	// watched now may be once a tag or the options change.
	Boxes map[string]*boxState `json:"boxes,omitempty"`
	// Requests are the watched boxes' secret requests' statuses, by ID.
	Requests map[string]string `json:"requests,omitempty"`
	// Gone are the boxes named by id that have been deleted.
	Gone []string `json:"gone,omitempty"`
	// Unconfirmed are the boxes held when a replay began — after a reset, or
	// from scratch over boxes a cut-off first replay left — which it has not
	// yet shown to exist; those it never shows were deleted.
	Unconfirmed []string `json:"unconfirmed,omitempty"`
	// Resetting is set from a reset until its replay is known to be whole,
	// even when it left nothing unconfirmed.
	Resetting bool `json:"resetting,omitempty"`
}

// boxState is a box as its last event left it.
type boxState struct {
	// Name is its configured name, the one its events carry.
	Name string `json:"name"`
	// State is its display state, from its existence and power.
	State string `json:"state"`
	// Program and Kind are what its primary terminal's program last
	// reported, while the box runs: idle, working, done, blocked or error.
	Program   string            `json:"program,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
	CreatedBy string            `json:"createdBy,omitempty"`
	// New is a box created after the baseline that has not yet been
	// reported created: it is when it is first watched.
	New bool `json:"new,omitempty"`
	// Watched is whether the options selected it when it was last looked at.
	Watched bool `json:"watched,omitempty"`
}

func (s *state) init() {
	if s.Boxes == nil {
		s.Boxes = map[string]*boxState{}
	}
	if s.Requests == nil {
		s.Requests = map[string]string{}
	}
}

// boxIDs are the held boxes' IDs, in order.
func (s *state) boxIDs() []string {
	return slices.Sorted(maps.Keys(s.Boxes))
}
