package boot

import (
	"testing"

	"github.com/obot-platform/discobox/sandboxconfig"
	"github.com/obot-platform/discobox/sandboxuser"
)

// The bug this pins: a sandbox whose manifest named no user had no source
// ownership to publish, that absence arrived as the zero value of a plain
// int64, and the wired source tree was chowned to root:root. An image running
// as a non-root USER then could not write its own checkout -- and this is the
// case ADR 0031 made ordinary rather than exotic.
//
// Absent is now absent, and boot answers with the identity it resolved, which
// it has in hand at that moment and which is the better answer regardless
// (ADR 0032 §5).
func TestSourceOwnerFallsBackToTheResolvedIdentity(t *testing.T) {
	id := identity{uid: 1500, gid: 1600, name: "image", home: "/home/image"}

	for _, tc := range []struct {
		name    string
		source  sandboxconfig.Source
		wantUID int
		wantGID int
	}{
		{
			// The manifest could not say, so boot does.
			name:    "no ownership stated",
			source:  sandboxconfig.Source{Slug: "primary", Target: "/workspace"},
			wantUID: 1500, wantGID: 1600,
		},
		{
			// The request stated them, so the manifest is the more specific
			// answer and wins.
			name:    "ownership stated",
			source:  sandboxconfig.Source{Slug: "primary", Target: "/workspace", UID: sandboxuser.ID(1000), GID: sandboxuser.ID(2000)},
			wantUID: 1000, wantGID: 2000,
		},
		{
			// Root is a legitimate choice, and telling it apart from "not given"
			// is the entire reason these fields are pointers.
			name:    "explicit root",
			source:  sandboxconfig.Source{Slug: "primary", Target: "/workspace", UID: sandboxuser.ID(0), GID: sandboxuser.ID(0)},
			wantUID: 0, wantGID: 0,
		},
		{
			// One id given and not the other is not a reason to discard the one
			// boot resolved for the other.
			name:    "only a uid stated",
			source:  sandboxconfig.Source{Slug: "primary", Target: "/workspace", UID: sandboxuser.ID(1000)},
			wantUID: 1000, wantGID: 1600,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, gid := sourceOwner(tc.source, id)
			if uid != tc.wantUID || gid != tc.wantGID {
				t.Fatalf("owner = %d:%d, want %d:%d", uid, gid, tc.wantUID, tc.wantGID)
			}
		})
	}
}
