package console

import (
	"sort"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sandboxservices"
)

// Header is what a workspace's header says of its box (ADR 26-11-07-876 §1):
// which box it is, the work it is tagged with, where that work sits in git,
// what it is serving and the rest of its tags. Each field is the core's
// answer; the order the fields go in, their colors and what a renderer gives
// up when it runs out of room are the renderer's.
//
// Everything in it is read off the box as the listing has it, so a header
// follows the listing; the ports also follow the workspace's forward, which
// lives as long as the screen showing it (Workspace.Park).
type Header struct {
	// ID is what identifies the box: what one would type at a shell to act
	// on it.
	ID string
	// Work is the issue and the pull request the box's tags number, each a
	// link to it on GitHub, in the order the tags are kept.
	Work []Link
	// Blocked is a box waiting on a person (Sandbox.Standing): said in place
	// of the git word, since waiting on somebody outranks what git says.
	Blocked bool
	// Base is the work's git position (Sandbox.Base); empty when nothing is
	// known.
	Base string
	// Changes is the work's state as a word (Sandbox.Changes): dirty, ready
	// or clean, and empty when there is nothing to say or Blocked says it.
	Changes string
	// Tone is how the git position and word read.
	Tone GitTone
	// Ready is a ready pull request over ready work: the two agree, so the
	// word reads as done rather than in Tone.
	Ready bool
	// Diff is what the box has changed, when it has reported a change.
	Diff *DiffStat
	// Desktop is the box's graphical desktop as a link to the local end of
	// its forward, once the forward has bound it.
	Desktop *Link
	// Ports is what the box is serving, grouped by protocol, web first.
	Ports []PortGroup
	// Tags is the box's tags the header says nothing of otherwise, as the
	// rows label them: not an issue or pull request Work links, and not a
	// status tag, which the git word says on terms of its own.
	Tags []string
}

// Link is something in a header a press opens. A Link without a URL is text.
type Link struct {
	Label string
	URL   string
	// Issue is the issue or pull request number a work link is to, and Pull
	// whether it is a pull request.
	Issue int
	Pull  bool
}

// GitTone is how a header reads the work's git position and word.
type GitTone int

const (
	// ToneQuiet is clean work, or work nothing is known about.
	ToneQuiet GitTone = iota
	// ToneDirty is uncommitted work.
	ToneDirty
	// ToneAhead is committed work beyond what the box was cut from.
	ToneAhead
)

// PortGroup is one protocol's ports, in port order.
type PortGroup struct {
	// Protocol is the protocol as a header labels it: `?` for one whose
	// probe has not answered.
	Protocol string
	Ports    []Link
}

// NewHeader is the header of a box, with its ports at the local ports
// forwarded names (nil before the workspace's forward has bound any).
func NewHeader(box Sandbox, forwarded map[PortKey]int) Header {
	h := Header{ID: box.ID, Work: box.WorkLinks(), Tags: box.HeaderTags(), Base: box.Base(), Tone: box.GitTone(), Diff: box.Changed()}
	h.Blocked = box.Standing() == StandingBlocked
	if changes := box.Changes(); changes != "-" && !h.Blocked {
		h.Changes = changes
		h.Ready = changes == "ready" && box.Standing() == StandingReady
	}
	h.Desktop = desktopLink(box, forwarded)
	h.Ports = portGroups(box, forwarded)
	return h
}

// GitTone is how the box's git position and word read, wherever they are
// drawn: the list's column and the header alike.
func (s Sandbox) GitTone() GitTone {
	switch {
	case s.Uncommitted():
		return ToneDirty
	case s.Ahead():
		return ToneAhead
	}
	return ToneQuiet
}

// Changed is what the box has changed, or nil when it has reported nothing
// or nothing has changed: the same answer to anything drawing it.
func (s Sandbox) Changed() *DiffStat {
	if !s.HasDiff() {
		return nil
	}
	d := s.Diff
	return &d
}

// WorkLinks is the issue and the pull request the box's `issue=` and `pr=`
// tags number, each a link to it on GitHub (Sandbox.TagURL): the work this
// box is doing, one press from what is being said about it. None when it has
// neither, or no GitHub repository for them to be numbered in.
func (s Sandbox) WorkLinks() []Link {
	var out []Link
	for _, tag := range s.Tags {
		url := s.TagURL(tag)
		if url == "" {
			continue
		}
		key, number, _ := strings.Cut(tag, "=")
		n, _ := strconv.Atoi(number)
		label := "issue #" + number
		if key == "pr" {
			label = "PR #" + number
		}
		out = append(out, Link{Label: label, URL: url, Issue: n, Pull: key == "pr"})
	}
	return out
}

// HeaderTags is the box's tags a header says nothing of otherwise, labeled
// as the rows label them, in the order they are kept: not an issue or pull
// request WorkLinks links, and not a status tag, which the header says
// through the git word on terms of its own that a tag beside it would
// contradict.
func (s Sandbox) HeaderTags() []string {
	var out []string
	for _, tag := range s.Tags {
		if s.TagURL(tag) == "" && !IsStatusTag(tag) {
			out = append(out, TagLabel(tag))
		}
	}
	return out
}

// desktopLink is the box's graphical desktop, as a link to the local end of
// its forward.
//
// It is apart from the protocol groups because it is a different kind of
// thing: a desktop to open, not a port to connect something to. It is known
// by the id the image declared it under (sandboxservices.DesktopID) rather
// than by a port number, and labeled with the declaration's own name. It is
// offered only once the forward has bound it, for the reason portLink links
// only forwarded ports: an offer to open a desktop that cannot be reached is
// worse than none.
func desktopLink(s Sandbox, forwarded map[PortKey]int) *Link {
	for _, port := range s.Ports {
		if port.ServiceID != sandboxservices.DesktopID {
			continue
		}
		local, ok := forwarded[port.Key()]
		if !ok {
			return nil
		}
		scheme, web := portScheme(port.Protocol)
		if !web {
			return nil
		}
		label := port.ServiceName
		if label == "" {
			label = "Desktop"
		}
		return &Link{Label: label, URL: scheme + "://localhost:" + strconv.Itoa(local)}
	}
	return nil
}

// portGroups is what the box is serving, grouped by protocol:
//
//	http:3000,5173,8080 · https:8443 · tcp:22,5432,6379 · udp:53,5353
//
// Grouped rather than one `protocol/port` per port, because the protocol is
// the repetitive half. The protocol leads its group because it decides
// whether a port is worth opening at all — `http:5173` is a page, `tcp:5432`
// a database — and the groups run in that order of usefulness, web first. A
// protocol this CLI does not know keeps its own name and follows them.
//
// The desktop is left out: it is its own offer (desktopLink), and listing
// its port beside a dev server invites opening it as if it were one.
func portGroups(s Sandbox, forwarded map[PortKey]int) []PortGroup {
	groups := map[string][]Port{}
	var order []string
	for _, port := range s.Ports {
		if port.ServiceID == sandboxservices.DesktopID {
			continue
		}
		if _, seen := groups[port.Protocol]; !seen {
			order = append(order, port.Protocol)
		}
		groups[port.Protocol] = append(groups[port.Protocol], port)
	}
	sort.SliceStable(order, func(i, j int) bool { return protocolRank(order[i]) < protocolRank(order[j]) })
	out := make([]PortGroup, 0, len(order))
	for _, protocol := range order {
		ports := groups[protocol]
		sort.Slice(ports, func(i, j int) bool { return ports[i].Number < ports[j].Number })
		g := PortGroup{Protocol: protocolLabel(protocol)}
		for _, port := range ports {
			g.Ports = append(g.Ports, portLink(port, forwarded))
		}
		out = append(out, g)
	}
	return out
}

// portLink is one port in its group: the number on its own, or
// `local->remote` when the forward had to move it, since the arrow answers
// "which number do I type here" only when the answer differs.
//
// A forwarded web port is also a link to the local end of it, so a page the
// box serves is one press away rather than a URL to assemble. Only the
// forwarded ones: a link to a port nothing is listening on is worse than
// none. Only the web ones: there is nothing for a browser to do with a
// Postgres socket.
func portLink(port Port, forwarded map[PortKey]int) Link {
	local, ok := forwarded[port.Key()]
	if !ok {
		return Link{Label: strconv.Itoa(port.Number)}
	}
	label := strconv.Itoa(local)
	if local != port.Number {
		label += "->" + strconv.Itoa(port.Number)
	}
	scheme, web := portScheme(port.Protocol)
	if !web {
		return Link{Label: label}
	}
	return Link{Label: label, URL: scheme + "://localhost:" + strconv.Itoa(local)}
}

// portScheme is the URL scheme a protocol is reachable under, and whether it
// is one at all. The forward is a byte pipe, so a port speaking https is https
// at the local end too, with a certificate whose name does not match: that is
// inherent to forwarding it.
func portScheme(protocol string) (string, bool) {
	switch protocol {
	case "http", "https":
		return protocol, true
	default:
		return "", false
	}
}

// protocolOrder is the order the groups are drawn in: what one would act on
// first. Anything not named here follows, in port order.
var protocolOrder = []string{"http", "https", "tcp", "udp", "unknown"}

func protocolRank(protocol string) int {
	for rank, known := range protocolOrder {
		if protocol == known {
			return rank
		}
	}
	return len(protocolOrder)
}

// protocolLabel is a protocol as a group is labeled: `unknown` is the
// longest word for the least information, and a port whose probe has not
// answered is where the number is all there is to say.
func protocolLabel(protocol string) string {
	if protocol == "" || protocol == "unknown" {
		return "?"
	}
	return protocol
}

// Forwarded is a forward's bindings as a header looks ports up in them: by
// port, the local port each landed on. Nil for a forward with none.
func Forwarded(bindings []Binding) map[PortKey]int {
	if len(bindings) == 0 {
		return nil
	}
	out := make(map[PortKey]int, len(bindings))
	for _, b := range bindings {
		out[b.Key()] = b.Local
	}
	return out
}
