package tui

import (
	"errors"
	"strings"
	"testing"
)

// imageGoneSandboxes is the fixture with its first box failed on an image its
// pool cannot obtain: no container, a latched error, and a newer image to move
// to when upgrade says so.
func imageGoneSandboxes(upgrade bool) []Sandbox {
	boxes := testSandboxes()
	boxes[0].State = StateError
	boxes[0].HasRuntime = false
	boxes[0].ImageUnavailable = true
	boxes[0].Upgrade = upgrade
	boxes[0].Message = `"harness:local" is not on this pool`
	return boxes
}

// Attaching to a box whose image is gone cannot work, so the window asks
// whether to upgrade it instead — and the answer is the upgrade, not an attach.
func TestAttachToABoxWhoseImageIsGoneOffersTheUpgrade(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(imageGoneSandboxes(true)...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("a"))

	if m.dialog == nil || m.dialog.kind != dlgConfirm || m.dialog.title != "Upgrade" {
		t.Fatalf("dialog = %+v, want the upgrade offer", m.dialog)
	}
	if m.inPanes() {
		t.Fatal("the attach went ahead against a box that cannot start")
	}
	send(t, m, keyPress("y"))
	if len(ds.did) != 1 || ds.did[0] != "upgrade sbx_one" {
		t.Fatalf("did = %v, want the upgrade", ds.did)
	}
}

// With no newer image there is nothing to offer, and the window says that
// rather than pointing at a repair that would rebuild on the same missing image.
func TestAttachToABoxWhoseImageIsGoneWithNothingToUpgradeToSaysSo(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(imageGoneSandboxes(false)...)
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress("a"))

	if m.dialog == nil || m.dialog.kind != dlgMessage || !strings.Contains(m.dialog.body, "no newer image") {
		t.Fatalf("dialog = %+v, want it to say there is no newer image", m.dialog)
	}
	if len(ds.did) != 0 {
		t.Fatalf("did = %v, want nothing run", ds.did)
	}
}

// The offer is made from the listing read for the failed attach, and only for
// the box that failed on its image. An ordinary listing raises nothing on its
// own, and neither does a listing that could not be read.
func TestAFailedAttachOffersTheUpgradeFromItsOwnListing(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)

	send(t, m, listLoadedMsg{listing: Listing{Sandboxes: imageGoneSandboxes(true)}})
	if m.dialog != nil {
		t.Fatal("a routine listing raised the offer with no attach to answer")
	}

	send(t, m, imageOfferListedMsg{id: "sbx_one", err: errors.New("server unreachable")})
	if m.dialog != nil {
		t.Fatal("a listing that failed raised an offer")
	}

	send(t, m, imageOfferListedMsg{id: "sbx_two", listing: Listing{Sandboxes: imageGoneSandboxes(true)}})
	if m.dialog != nil {
		t.Fatal("an attach that failed for another reason was offered an upgrade")
	}

	send(t, m, imageOfferListedMsg{id: "sbx_one", listing: Listing{Sandboxes: imageGoneSandboxes(true)}})
	if m.dialog == nil || m.dialog.title != "Upgrade" {
		t.Fatalf("dialog = %+v, want the upgrade offer", m.dialog)
	}
}

// The whole path: the row still looks healthy when attach is pressed, and the
// server records the failure only as the attach fails. The offer arrives
// without the attach being pressed again.
func TestAnAttachThatFailsOnAMissingImageEndsInTheOffer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	m := New(t.Context(), ds)
	m.logo = logo{}
	d := newDriver(t, m)
	d.start()
	d.wait("the listing", func() bool { return len(m.list.rows()) > 0 })

	ds.mu.Lock()
	ds.openExecErr = errors.New("sandbox failed")
	ds.failedExecSandboxes = imageGoneSandboxes(true)
	ds.mu.Unlock()
	d.key("tab")
	d.key("a")

	d.wait("the upgrade offer", func() bool { return m.dialog != nil && m.dialog.title == "Upgrade" })
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if len(ds.execOpens) == 0 {
		t.Fatal("the offer came from the row rather than from the failed attach this is about")
	}
}
