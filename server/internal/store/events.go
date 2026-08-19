package store

import (
	"context"

	"github.com/obot-platform/discobox/server/internal/model"
)

// publishProjectEvent fans a committed resource event out to the in-process
// broker (ADR 0039), or holds it until the surrounding transaction commits.
//
// The broker is the only consumer. The client-facing stream that once read
// these back is gone (ADR 0061), so nothing lists or resumes them; a subscriber
// takes what it is handed and re-reads the resource to be sure.
func (s *Store) publishProjectEvent(ctx context.Context, event model.ProjectEvent) {
	if s.afterCommitEvents != nil {
		*s.afterCommitEvents = append(*s.afterCommitEvents, event)
		return
	}
	if s.publisher != nil {
		s.publisher.PublishProjectEvent(ctx, event)
	}
}
