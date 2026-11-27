package handover

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// A probe takes nothing: probes running all the while never make a lock or a
// mark of the file fail to be taken, nor see the free lock as held.
func TestProbesNeverTakeTheLockTheyAsk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot")
	release, err := Mark(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = release.Release()

	ctx, stop := context.WithCancel(context.Background())
	var probes sync.WaitGroup
	for range 4 {
		probes.Go(func() {
			for ctx.Err() == nil {
				if _, err := Held(path); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	for range 500 {
		lock, ok, err := TryAcquire(path)
		if err != nil || !ok {
			stop()
			probes.Wait()
			t.Fatalf("TryAcquire beside probes = %v, %v; a probe took the lock", ok, err)
		}
		_ = lock.Release()
	}
	stop()
	probes.Wait()

	// With nothing held, no probe says held, however many run at once.
	ctx, stop = context.WithCancel(context.Background())
	defer stop()
	falseHeld := make(chan struct{}, 1)
	for range 8 {
		probes.Go(func() {
			for range 200 {
				if held, _ := Held(path); held {
					select {
					case falseHeld <- struct{}{}:
					default:
					}
				}
			}
		})
	}
	probes.Wait()
	select {
	case <-falseHeld:
		t.Fatal("a probe of a free lock said held: probes saw each other")
	default:
	}
}
