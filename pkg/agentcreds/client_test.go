package agentcreds

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitForKeepsPollFailureWhenDeadlineInterruptsNextPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		known := RequestStatus{RequestID: "existing", Status: StatusPending}
		polls := 0
		_, err := waitFor(ctx, known, false, time.Second, func() (RequestStatus, bool, error) {
			polls++
			if polls == 1 {
				return RequestStatus{}, false, ErrUnavailable
			}
			<-ctx.Done()
			return RequestStatus{}, false, fmt.Errorf("poll interrupted: %w", ctx.Err())
		})
		if polls != 2 || !errors.Is(err, ErrUnavailable) {
			t.Fatalf("wait returned %v after %d polls, want unavailable after two polls", err, polls)
		}
	})
}
