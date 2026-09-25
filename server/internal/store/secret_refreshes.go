package store

import (
	"context"
	"sort"
	"time"

	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// SecretRefreshFilter narrows ListSecretRefreshEvents. A zero field matches
// everything.
type SecretRefreshFilter struct {
	// ID reads the events of the one refresh request it names.
	ID string
	// SandboxID is matched against the recorded ID, for the reason
	// CredentialVerdictFilter's is: the trail outlives the discobox. It is the
	// discobox that most recently needed the value.
	SandboxID string
	SecretID  string
	// Since and Until bound the events, not the requests: a request asked
	// before Since whose answer came after it contributes its answer.
	Since time.Time
	Until time.Time
	// Ascending returns the oldest events first, for a follower reading
	// forward from Since.
	Ascending bool
	// Limit caps the events returned; zero returns every match.
	Limit int
}

// Bounds standing in for an unset Since or Until. Times are stored in UTC
// (SecretRequest.BeforeCreate, closedAt), so these compare correctly as text.
var (
	refreshTrailBeginning = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	refreshTrailEnd       = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ListSecretRefreshEvents returns the refresh trail's events that match
// filter, newest first unless the filter reads forward (ADR 26-09-25-122 §6).
// Each refresh request is its asking and, once closed, its answer or its
// dismissal.
//
// The limit is applied to requests in SQL and to events here, which returns
// exactly the first Limit events: each request is ordered by its event nearest
// the reading edge inside the bounds, and any event among the first Limit has a
// request whose nearest event is at least as near, of which there are at most
// Limit.
func (s *Store) ListSecretRefreshEvents(ctx context.Context, projectID string, filter SecretRefreshFilter) ([]model.SecretRefreshEvent, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	since, until := filter.Since.UTC(), filter.Until.UTC()
	if filter.Since.IsZero() {
		since = refreshTrailBeginning
	}
	if filter.Until.IsZero() {
		until = refreshTrailEnd
	}
	query := read.Model(&model.SecretRequest{}).
		Where("project_id = ? AND reason = ?", projectID, model.SecretRequestReasonRefresh).
		Where("(created_at >= ? AND created_at <= ?) OR (closed_at IS NOT NULL AND closed_at >= ? AND closed_at <= ?)", since, until, since, until)
	if filter.ID != "" {
		query = query.Where("id = ?", filter.ID)
	}
	if filter.SandboxID != "" {
		query = query.Where("sandbox_id = ?", filter.SandboxID)
	}
	if filter.SecretID != "" {
		query = query.Where("secret_id = ?", filter.SecretID)
	}
	if filter.Ascending {
		// A request's earliest event inside the bounds: its asking, unless
		// that came before them.
		query = query.Order(orderByExpr("CASE WHEN created_at >= ? THEN created_at ELSE closed_at END ASC, id ASC", since))
	} else {
		// Its latest event inside the bounds: its closing, unless that is
		// past them or has not happened.
		query = query.Order(orderByExpr("CASE WHEN closed_at IS NOT NULL AND closed_at <= ? THEN closed_at ELSE created_at END DESC, id DESC", until))
	}
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	var rows []model.SecretRequest
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}

	var events []model.SecretRefreshEvent
	within := func(at time.Time) bool { return !at.Before(since) && !at.After(until) }
	for _, row := range rows {
		base := model.SecretRefreshEvent{
			ID:           row.ID,
			ProjectID:    row.ProjectID,
			SecretID:     row.SecretID,
			SandboxID:    row.SandboxID,
			RefreshCause: row.RefreshCause,
		}
		if within(row.CreatedAt) {
			asked := base
			asked.Event, asked.At = model.SecretRefreshEventAsked, row.CreatedAt.UTC()
			events = append(events, asked)
		}
		if row.ClosedAt == nil || !within(*row.ClosedAt) || row.Status == model.SecretRequestStatusPending {
			continue
		}
		closed := base
		closed.At = row.ClosedAt.UTC()
		switch row.Status {
		case model.SecretRequestStatusDenied:
			closed.Event = model.SecretRefreshEventDismissed
		default:
			closed.Event = model.SecretRefreshEventAnswered
			closed.Answer = row.RefreshAnswer
		}
		events = append(events, closed)
	}
	// An asking sorts before the answer it shares an instant with, and two
	// requests in one instant sort by ID, so two reads agree on the order.
	rank := func(e model.SecretRefreshEvent) int {
		if e.Event == model.SecretRefreshEventAsked {
			return 0
		}
		return 1
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At) == filter.Ascending
		}
		if a.ID != b.ID {
			return (a.ID < b.ID) == filter.Ascending
		}
		return (rank(a) < rank(b)) == filter.Ascending
	})
	if filter.Limit > 0 && len(events) > filter.Limit {
		events = events[:filter.Limit]
	}
	return events, nil
}

// orderByExpr is the whole ORDER BY, binding a value. It has to be the whole of
// it: a further Order call merges into an expression clause by replacing it.
func orderByExpr(sql string, vars ...any) clause.OrderBy {
	return clause.OrderBy{Expression: clause.Expr{SQL: sql, Vars: vars, WithoutParentheses: true}}
}
