package discoboxbox

import "time"

// state is what one run leaves the next.
type state struct {
	// Baseline is when the first run recorded where things stood, on the
	// box's clock. Zero before it.
	Baseline time.Time `json:"baseline"`
	// Boxes are the watched boxes as last seen, by ID.
	Boxes map[string]*boxState `json:"boxes,omitempty"`
	// Requests are the watched boxes' secret requests' statuses, by ID.
	Requests map[string]string `json:"requests,omitempty"`
	// Gone are the boxes named by id that have left the project.
	Gone []string `json:"gone,omitempty"`
}

type boxState struct {
	Name string `json:"name"`
	// State is the box's displayState.
	State string `json:"state"`
	// Program, Kind and Msg are what its primary terminal's program last
	// reported, while the box runs: idle, working, done, blocked or error.
	Program string `json:"program,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Msg     string `json:"msg,omitempty"`
}

func (s *state) init() {
	if s.Boxes == nil {
		s.Boxes = map[string]*boxState{}
	}
	if s.Requests == nil {
		s.Requests = map[string]string{}
	}
}
