package secretroute

import (
	"reflect"
	"testing"
)

// A legacy route FromLegacy refuses is read without widening: an open one
// goes, a blocked one blocks its path cut back before the segment no path
// can spell, and a blocked one whose host does not read refuses its list.
func TestFromLegacyClosed(t *testing.T) {
	hosts := []string{"*.github.com", "github.com"}
	for _, tc := range []struct {
		host, path        string
		blocked           bool
		want              Route
		wantOK, wantRefus bool
	}{
		{host: "github.com", path: "/repos", want: Route{Hosts: hosts, Paths: []string{"/repos/**"}}, wantOK: true},
		{host: "github.com", path: "/repos/a*b"},
		{host: "github.com", path: "/repos/acme/we*b/x", blocked: true, want: Route{Hosts: hosts, Paths: []string{"/repos/acme/**"}, Blocked: true}, wantOK: true},
		{host: "github.com", path: "/a=b/c", blocked: true, want: Route{Hosts: hosts, Blocked: true}, wantOK: true},
		{host: "github.com", path: "/x,/y", blocked: true, want: Route{Hosts: hosts, Blocked: true}, wantOK: true},
		{host: "git,hub.com", path: "/x", blocked: true, wantRefus: true},
		{host: "git,hub.com", path: "/x"},
	} {
		got, ok, refuse := fromLegacyClosed(tc.host, tc.path, tc.blocked)
		if ok != tc.wantOK || refuse != tc.wantRefus || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("fromLegacyClosed(%q, %q, %v) = %+v, %v, %v; want %+v, %v, %v", tc.host, tc.path, tc.blocked, got, ok, refuse, tc.want, tc.wantOK, tc.wantRefus)
		}
	}
}

// The cut-back block still refuses everything the legacy block did, with the
// open routes beside it that FromLegacy converts.
func TestFromLegacyClosedRefusesWhatTheBlockDid(t *testing.T) {
	var list []Route
	for _, legacy := range []struct {
		path    string
		blocked bool
	}{{"", false}, {"/a/m", false}, {"/a/m/x*y", true}, {"/a/m/x*y/z", false}} {
		if route, ok, _ := fromLegacyClosed("h.example", legacy.path, legacy.blocked); ok {
			list = append(list, route)
		}
	}
	for _, tc := range []struct {
		url     string
		allowed bool
	}{
		{"https://h.example/a/m/x*y", false},
		{"https://h.example/a/m/x*y/z", false},
		{"https://h.example/a/m/x*y/z/w", false},
		{"https://h.example/a", true},
		{"https://h.example/b", true},
	} {
		if allowed, reason := Decide([][]Route{list}, Request{Method: "GET", Host: "h.example", URL: tc.url}); allowed != tc.allowed {
			t.Errorf("%s: allowed = %v (%s), want %v", tc.url, allowed, reason, tc.allowed)
		}
	}
}

func TestFromLegacyList(t *testing.T) {
	hosts := []string{"*.github.com", "github.com"}
	for _, tc := range []struct {
		name      string
		in        []LegacyRoute
		current   []Route
		want      []Route
		wantNotes int
	}{
		{name: "empty stays empty", in: nil, want: nil},
		{
			name: "exact conversions, repeats collapsed",
			in:   []LegacyRoute{{Host: "github.com"}, {Host: "GitHub.com"}, {Host: "github.com", Path: "/admin", Blocked: true}},
			want: []Route{{Hosts: hosts}, {Hosts: hosts, Paths: []string{"/admin/**"}, Blocked: true}},
		},
		{
			name:      "an unreadable open route is dropped",
			in:        []LegacyRoute{{Host: "github.com"}, {Host: "github.com", Path: "/a*b"}},
			want:      []Route{{Hosts: hosts}},
			wantNotes: 1,
		},
		{
			name:      "an unreadable blocked path is cut back",
			in:        []LegacyRoute{{Host: "github.com"}, {Host: "github.com", Path: "/repos/a*b/x", Blocked: true}},
			want:      []Route{{Hosts: hosts}, {Hosts: hosts, Paths: []string{"/repos/**"}, Blocked: true}},
			wantNotes: 1,
		},
		{
			name:      "a blocked route on an unreadable host refuses the list",
			in:        []LegacyRoute{{Host: "github.com"}, {Host: "git,hub.com", Blocked: true}, {Host: "example.com"}},
			want:      RefusingList(),
			wantNotes: 1,
		},
		{
			// A row both shapes were written to keeps its current routes
			// when every legacy one drops: they are exactly as narrow.
			name:      "a mixed list whose legacy part all drops",
			in:        []LegacyRoute{{Host: "api.github.com", Path: "/a*b"}},
			current:   []Route{{Hosts: []string{"api.github.com"}}},
			want:      []Route{{Hosts: []string{"api.github.com"}}},
			wantNotes: 1,
		},
		{
			name:    "a mixed list converts beside its current routes, repeats collapsed",
			in:      []LegacyRoute{{Host: "github.com"}},
			current: []Route{{Hosts: []string{"api.github.com"}}, {Hosts: hosts}},
			want:    []Route{{Hosts: []string{"api.github.com"}}, {Hosts: hosts}},
		},
		{
			name:      "a mixed list still refuses on a blocked route that does not read",
			in:        []LegacyRoute{{Host: "git,hub.com", Blocked: true}},
			current:   []Route{{Hosts: []string{"api.github.com"}}},
			want:      RefusingList(),
			wantNotes: 1,
		},
		{
			name:    "current routes alone are kept",
			current: []Route{{Hosts: []string{"api.github.com"}}},
			want:    []Route{{Hosts: []string{"api.github.com"}}},
		},
		{
			// A binding left with no routes would inherit its secret's,
			// which is wider than the one route it held.
			name:      "a list emptied by dropping refuses",
			in:        []LegacyRoute{{Host: "api.github.com", Path: "/repos/acme/*"}},
			want:      RefusingList(),
			wantNotes: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, notes := FromLegacyList(tc.in, tc.current)
			if !reflect.DeepEqual(got, tc.want) || len(notes) != tc.wantNotes {
				t.Fatalf("FromLegacyList(%+v, %+v) = %+v, %q; want %+v and %d notes", tc.in, tc.current, got, notes, tc.want, tc.wantNotes)
			}
		})
	}
	if allowed, _ := Decide([][]Route{RefusingList()}, Request{Method: "GET", Host: "example.com", URL: "https://example.com/"}); allowed {
		t.Fatal("RefusingList allowed a request")
	}
	if HostAllowed([][]Route{RefusingList()}, "example.com") {
		t.Fatal("RefusingList allowed a host")
	}
}
