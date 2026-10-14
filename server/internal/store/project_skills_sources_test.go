package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// The list is in the order its entries were added even when they share a
// creation time, which a coarse clock (Windows') gives several adds in a row.
func TestProjectSkillsSourcesKeepTheirOrderOnOneClockTick(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	at := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	var want []string
	for _, url := range []string{"https://example.com/z", "https://example.com/a", "https://example.com/m"} {
		source := &model.ProjectSkillsSource{ProjectID: "project-1", URL: url, Directory: ".", CreatedAt: at}
		if err := s.CreateProjectSkillsSource(ctx, source); err != nil {
			t.Fatalf("create %s: %v", url, err)
		}
		want = append(want, source.ID)
	}
	list, err := s.ListProjectSkillsSources(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != len(want) {
		t.Fatalf("list = %+v, want %d entries", list, len(want))
	}
	for i, source := range list {
		if source.ID != want[i] || !source.CreatedAt.Equal(at) {
			t.Fatalf("list[%d] = %s at %v, want %s at %v", i, source.ID, source.CreatedAt, want[i], at)
		}
	}
}
