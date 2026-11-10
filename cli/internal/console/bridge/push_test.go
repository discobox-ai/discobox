package bridge

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// pushSnapshot is the next snapshot of the push topic's subscription sub.
func (a *app) pushSnapshot(sub uint32) Push {
	a.t.Helper()
	for {
		snap := a.expect(TypeSnapshot).Snapshot
		if snap.Sub != sub {
			continue
		}
		if snap.Topic != TopicPush || snap.Push == nil {
			a.t.Fatalf("push snapshot %+v", snap)
		}
		return *snap.Push
	}
}

// The push topic is the core's push loop on the bridge's clock: a commit made
// here goes into the box's origin within a beat, saying where it landed; a
// refusal is said once, as a warning, and held until a new commit; and the
// loop ends with the subscription, as the app lets the workspace go.
func TestThePushTopicRunsTheCoresPushLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newFakeCore("home", console.Sandbox{ID: "b1", Name: "api", State: console.StateRunning, HasRuntime: true, Pushable: true})
		core.commit("c1aaaaaaaaaaaaaa", nil)
		a := dial(t, core)
		a.hello()
		a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
		a.expect(TypeSnapshot)

		a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicPush, Box: "b1"}})
		if p := a.pushSnapshot(2); p != (Push{}) {
			t.Fatalf("first snapshot %+v, want nothing said", p)
		}
		want := Push{Said: 1, Text: "pushed c1aaaaaaaaaa to origin/main in the discobox", Elsewhere: "api: pushed c1aaaaaaaaaa to origin/main in the discobox"}
		if p := a.pushSnapshot(2); p != want {
			t.Fatalf("pushed %+v, want %+v", p, want)
		}

		// Nothing new to send says nothing.
		a.quiet(2 * console.PushEvery)

		// A refusal is a warning, said once, and held at its commit.
		core.commit("c2bbbbbbbbbbbbbb", errors.New("the lease moved"))
		if p := a.pushSnapshot(2); p != (Push{Said: 2, Text: "the lease moved", Warning: true, Elsewhere: "api: the lease moved"}) {
			t.Fatalf("refused %+v", p)
		}
		a.quiet(3 * console.PushEvery)
		looks := core.looks()
		if last := looks[len(looks)-1]; last["primary"] != "c2bbbbbbbbbbbbbb" {
			t.Fatalf("the next look held %v, want the refused commit", last)
		}

		// A new commit is tried again.
		core.commit("c3cccccccccccccc", nil)
		if p := a.pushSnapshot(2); p.Said != 3 || p.Text != "pushed c3cccccccccc to origin/main in the discobox" || p.Warning {
			t.Fatalf("pushed after a refusal %+v", p)
		}

		// Unsubscribed, it looks no more.
		a.send(Message{Type: TypeUnsubscribe, Unsubscribe: &Unsubscribe{Sub: 2}})
		synctest.Wait()
		n := len(core.looks())
		a.quiet(3 * console.PushEvery)
		if got := len(core.looks()); got != n {
			t.Fatalf("looked %d times after the subscription ended", got-n)
		}
	})
}

// A box this window does not push — another machine's, a clone-delivered
// source — is never looked at, and the topic says nothing.
func TestThePushTopicLeavesABoxItDoesNotPush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		core := newFakeCore("home", console.Sandbox{ID: "b1", Name: "api", State: console.StateRunning, HasRuntime: true})
		core.commit("c1aaaaaaaaaaaaaa", nil)
		a := dial(t, core)
		a.hello()
		a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
		a.expect(TypeSnapshot)
		a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 2, Topic: TopicPush, Box: "b1"}})
		if p := a.pushSnapshot(2); p != (Push{}) {
			t.Fatalf("first snapshot %+v", p)
		}
		a.quiet(3 * console.PushEvery)
		if looks := core.looks(); len(looks) != 0 {
			t.Fatalf("looked %d times at a box this window does not push", len(looks))
		}
	})
}
