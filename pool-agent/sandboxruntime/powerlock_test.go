package sandboxruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
)

// holdPower holds sandboxID's power lock for the test, and returns what lets
// go of it.
func holdPower(t *testing.T, r interface {
	lockPower(context.Context, string) (func(), error)
}, sandboxID string) func() {
	t.Helper()
	unlock, err := r.lockPower(context.Background(), sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}

// Two agents of one pool, as during a handover, share each sandbox's power
// lock: an operation the old agent still holds it for keeps the new one's
// waiting, and it is the new one's once the old lets go.
func TestThePowerLockHoldsAcrossTwoAgentsOfAPool(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	old := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	next := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}

	release := holdPower(t, old, "sbx_1")
	if _, locked, _ := next.tryLockPower("sbx_1"); locked {
		t.Fatal("the new agent took a power lock the old one holds")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := next.lockPower(ctx, "sbx_1"); err == nil {
		t.Fatal("the new agent's wait for the lock ended while the old one held it")
	}
	release()
	unlock := holdPower(t, next, "sbx_1")
	defer unlock()
	if _, locked, _ := old.tryLockPower("sbx_1"); locked {
		t.Fatal("the old agent took the lock back from the new one")
	}
}

// A boot the old agent of a handover has under way is one the new agent
// sees and waits out, by the mark on disk.
func TestABootUnderWayInTheOtherAgentIsSeenAndWaitedOut(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	old := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	next := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}

	if next.SandboxBooting("sbx_1") {
		t.Fatal("a sandbox nobody boots reads booting")
	}
	boot := old.beginBoot("sbx_1")
	if !next.SandboxBooting("sbx_1") {
		t.Fatal("the new agent does not see the old one's boot")
	}
	waited := make(chan error, 1)
	go func() {
		booting, err := next.awaitBoot(context.Background(), "sbx_1")
		if err == nil && !booting {
			t.Error("awaitBoot found no boot under way")
		}
		waited <- err
	}()
	select {
	case err := <-waited:
		t.Fatalf("the wait ended while the boot was under way: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	old.endBoot("sbx_1", boot, nil)
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end with the boot")
	}
	if next.SandboxBooting("sbx_1") {
		t.Fatal("a boot that ended still reads booting")
	}
}

// Overlapping boots of one sandbox hold its mark until the newest ends, on
// disk as in this process: an older boot still waiting holds none, so no
// process reads the sandbox as booting once the newest has ended.
func TestTheBootMarkEndsWithTheNewestBoot(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	core := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	other := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	older := core.beginBoot("sbx_1")
	newer := core.beginBoot("sbx_1")
	core.endBoot("sbx_1", newer, nil)
	if core.SandboxBooting("sbx_1") {
		t.Fatal("the sandbox reads booting in its own process after the newest boot ended")
	}
	if other.SandboxBooting("sbx_1") {
		t.Fatal("another process reads the sandbox booting after the newest boot ended")
	}
	core.endBoot("sbx_1", older, nil)
	if other.SandboxBooting("sbx_1") || core.SandboxBooting("sbx_1") {
		t.Fatal("the sandbox reads booting once both boots ended")
	}
}

// Overlapping boots that end oldest first hold the mark until the newest ends.
func TestOverlappingBootsHoldTheMarkUntilTheLastEnds(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	core := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	other := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	first := core.beginBoot("sbx_1")
	second := core.beginBoot("sbx_1")
	core.endBoot("sbx_1", first, nil)
	if !other.SandboxBooting("sbx_1") {
		t.Fatal("the mark ended with the older of two boots")
	}
	core.endBoot("sbx_1", second, nil)
	if other.SandboxBooting("sbx_1") {
		t.Fatal("the mark outlived the last boot")
	}
}

// A removed sandbox leaves no lock file behind, and a waiter for its lock
// while it was removed holds the lock of the file the path names afterwards,
// the one any later locker takes too.
func TestARemovedSandboxsLocksAreReclaimedWithoutSplittingTheLock(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	old := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	next := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	release := holdPower(t, old, "sbx_1")
	boot := old.beginBoot("sbx_1")
	old.endBoot("sbx_1", boot, nil)
	waited := make(chan func(), 1)
	go func() {
		unlock, err := next.lockPower(context.Background(), "sbx_1")
		if err != nil {
			t.Error(err)
		}
		waited <- unlock
	}()
	time.Sleep(100 * time.Millisecond)
	old.forgetLocks("sbx_1")
	release()
	unlock := <-waited
	if _, locked, _ := old.tryLockPower("sbx_1"); locked {
		t.Fatal("the lock split in two over a removed file")
	}
	unlock()
	old.forgetLocks("sbx_1")
	entries, err := os.ReadDir(root.PoolLocks("prj", "pool"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a removed sandbox left %v in the lock tree", entries)
	}
}

// A power lock whose file cannot be made is an error, never "held elsewhere":
// a caller that steps aside from a held lock would step aside for good.
func TestALockTreeThatCannotBeMadeIsAnError(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	core := &sandboxCore{root: root, projectID: "prj", poolID: "pool"}
	dir := root.PoolLocks("prj", "pool")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file where the lock tree's directory goes.
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, locked, err := core.tryLockPower("sbx_1")
	if err == nil || locked || unlock != nil {
		t.Fatalf("tryLockPower = %v, %v; want an error", locked, err)
	}
	// The semaphore was given back: a lock that can be made again is taken.
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	unlock, locked, err = core.tryLockPower("sbx_1")
	if err != nil || !locked {
		t.Fatalf("tryLockPower once the tree can be made = %v, %v", locked, err)
	}
	unlock()
}
