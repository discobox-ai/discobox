package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
)

// A box whose pool cannot obtain the image it is pinned to fails the same way
// on every attach: the image is not coming back, and nothing about trying again
// changes that. The way out is an upgrade, which re-pins it to its harness's
// current image and rebuilds it there with its work intact. So when an attach
// meets that failure — before it starts, from the row, or after it fails, from
// the listing that follows — the window says so and offers the upgrade, rather
// than reporting a transport error and leaving the next move to be guessed.

// opensBox reports whether an action needs the box's container: the ones an
// unavailable image stops.
func opensBox(key string) bool {
	action, ok := interactions[key]
	return ok && (action == InteractAttach || action == InteractShell)
}

// offerImageUpgrade puts the upgrade to the user for a box whose image is gone,
// or says there is nothing to upgrade to.
func (m *Model) offerImageUpgrade(box Sandbox) {
	if !box.Upgrade {
		body := fmt.Sprintf("%s's image is no longer available on its pool, so it cannot start, and its harness has no newer image to move it to.", box.Name)
		if box.Message != "" {
			body += "\n\n" + box.Message
		}
		m.dialog = errorDialog("Cannot start "+box.Name, body)
		return
	}
	question := fmt.Sprintf("%s's image is no longer available on its pool, so it cannot start. Upgrade it to its harness's current image? Its work is kept.", box.Name)
	ids := []string{box.ID}
	m.dialog = confirmDialog("Upgrade", question, func(string) tea.Cmd {
		return func() tea.Msg { return runVerbMsg{verb: VerbUpgrade, ids: ids} }
	})
}

// imageOfferListedMsg is the listing read for one failed attach.
type imageOfferListedMsg struct {
	id      string
	listing Listing
	err     error
}

// checkImageOffer reads a listing for the box whose attach just failed.
//
// It is its own read rather than whichever listing arrives next, because the
// next one may have been asked for before the failure was recorded — the poll's,
// already on its way — and would say nothing about it. A read issued from here
// was issued after the attach failed, and the attach wait gives up only once
// the failure is on the row.
func (m *Model) checkImageOffer(id string) tea.Cmd {
	ctx, ds := m.ctx, m.ds
	return func() tea.Msg {
		listing, err := ds.List(ctx)
		return imageOfferListedMsg{id: id, listing: listing, err: err}
	}
}

// imageOfferListed makes the offer when the listing says the box failed on its
// image. A listing that could not be read offers nothing: the attach error is
// already on the status line, and a guess is worse than no offer.
func (m *Model) imageOfferListed(msg imageOfferListedMsg) {
	if msg.err != nil || m.dialog != nil {
		return
	}
	for _, box := range msg.listing.Sandboxes {
		if box.ID == msg.id && box.ImageUnavailable {
			m.offerImageUpgrade(box)
			return
		}
	}
}
