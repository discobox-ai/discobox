package jobs

import (
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/reconcile"
)

func TestJobFromMarkStatus(t *testing.T) {
	worker := "node-1"
	boom := "boom"
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name  string
		mark  reconcile.DirtyResource
		want  string
		error string
	}{
		{name: "claimed", mark: reconcile.DirtyResource{ClaimedBy: &worker, NotBefore: past}, want: "running"},
		{name: "claimable", mark: reconcile.DirtyResource{NotBefore: past}, want: "pending"},
		{name: "reconciler timer is not a failure", mark: reconcile.DirtyResource{NotBefore: future}, want: "scheduled"},
		{name: "failure backoff carries the error", mark: reconcile.DirtyResource{NotBefore: future, Attempts: 2, LastError: &boom}, want: "backoff", error: "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mark.ResourceType = "sandbox"
			tc.mark.ResourceID = "proj_1/sbx_1"
			job := jobFromMark(tc.mark)
			if job.Status != tc.want {
				t.Fatalf("status = %q, want %q", job.Status, tc.want)
			}
			got := ""
			if job.Error != nil {
				got = *job.Error
			}
			if got != tc.error {
				t.Fatalf("error = %q, want %q", got, tc.error)
			}
			if job.ResourceID != "sbx_1" || job.ID != "sandbox:proj_1:sbx_1" {
				t.Fatalf("id/resource = %q/%q", job.ID, job.ResourceID)
			}
		})
	}
}
