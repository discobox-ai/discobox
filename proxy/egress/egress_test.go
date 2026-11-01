package egress

import (
	"slices"
	"testing"
)

func TestMerge(t *testing.T) {
	tests := []struct {
		name   string
		levels []Policy
		want   Policy
	}{
		{name: "no levels", want: Policy{}},
		{
			name:   "an allow list alone does not restrict",
			levels: []Policy{{Allow: []string{"api.anthropic.com"}}, {}, {}},
			want:   Policy{Allow: []string{"api.anthropic.com"}},
		},
		{
			name:   "any level restricts",
			levels: []Policy{{}, {}, {DenyByDefault: true}},
			want:   Policy{DenyByDefault: true},
		},
		{
			name: "restricted reaches the union, normalized",
			levels: []Policy{
				{Allow: []string{"GitHub.com", " registry.npmjs.org "}, DenyByDefault: true},
				{Allow: []string{"github.com", "10.0.0.0/8"}},
				{Allow: []string{"api.anthropic.com", ""}},
			},
			want: Policy{Allow: []string{"10.0.0.0/8", "api.anthropic.com", "github.com", "registry.npmjs.org"}, DenyByDefault: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Merge(tt.levels...)
			if got.DenyByDefault != tt.want.DenyByDefault || !slices.Equal(got.Allow, tt.want.Allow) {
				t.Fatalf("Merge() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateEntry(t *testing.T) {
	for _, entry := range []string{"github.com", "*.googleapis.com", "registry.*", "*", "1.2.3.4", "10.0.0.0/8", "::1", "fd00::/8", "my_host.internal"} {
		if err := ValidateEntry(entry); err != nil {
			t.Errorf("ValidateEntry(%q) = %v, want nil", entry, err)
		}
	}
	for _, entry := range []string{"::ffff:8.0.0.0/101", "::ffff:1.2.3.4", "", " ", "github.com:443", "https://github.com", "*github.com", "a.*.com", "10.0.0.0/33", "a..b", "**.x"} {
		if err := ValidateEntry(entry); err == nil {
			t.Errorf("ValidateEntry(%q) = nil, want an error", entry)
		}
	}
}

func TestMatcherAllows(t *testing.T) {
	m := NewMatcher([]string{"github.com", "*.googleapis.com", "registry.*", "10.0.0.0/8", "1.2.3.4"})
	for _, host := range []string{"github.com", "GITHUB.com:443", "storage.googleapis.com", "registry.example", "10.1.2.3:22", "1.2.3.4", "github.com."} {
		if !m.Allows(host) {
			t.Errorf("Allows(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"api.github.com", "googleapis.com", "example.com", "11.0.0.1", "1.2.3.5", "[::1]:443"} {
		if m.Allows(host) {
			t.Errorf("Allows(%q) = true, want false", host)
		}
	}
	if NewMatcher([]string{"localhost"}).Allows("127.0.0.1") {
		t.Error("a domain entry matched an IP literal")
	}
	if NewMatcher([]string{"*"}).Allows("10.1.2.3") {
		t.Error(`"*" matched an IP literal`)
	}
}

func TestCovers(t *testing.T) {
	restricted := func(allow ...string) Policy { return Policy{Allow: allow, DenyByDefault: true} }
	tests := []struct {
		name         string
		outer, child Policy
		want         bool
	}{
		{name: "unrestricted creator covers anything", outer: Policy{}, child: Policy{}, want: true},
		{name: "restricted creator, unrestricted child", outer: restricted("github.com"), child: Policy{Allow: []string{"github.com"}}, want: false},
		{name: "same list", outer: restricted("github.com"), child: restricted("github.com"), want: true},
		{name: "narrower list", outer: restricted("github.com", "x.com"), child: restricted("github.com"), want: true},
		{name: "empty child list", outer: restricted(), child: restricted(), want: true},
		{name: "wider list", outer: restricted("github.com"), child: restricted("github.com", "evil.com"), want: false},
		{name: "wildcard covers a name beneath it", outer: restricted("*.github.com"), child: restricted("api.github.com", "*.x.github.com"), want: true},
		{name: "wildcard does not cover its apex", outer: restricted("*.github.com"), child: restricted("github.com"), want: false},
		{name: "suffix wildcard does not cover a prefix wildcard", outer: restricted("*.com"), child: restricted("registry.*"), want: false},
		{name: "prefix wildcard covers a narrower one", outer: restricted("registry.*"), child: restricted("registry.example", "registry.x.*"), want: true},
		{name: "star covers every name", outer: restricted("*"), child: restricted("anything.example", "*.x.example", "registry.*"), want: true},
		{name: "star covers no IP, as it matches none", outer: restricted("*"), child: restricted("10.0.0.0/8"), want: false},
		{name: "star covers no IP literal", outer: restricted("*"), child: restricted("1.2.3.4"), want: false},
		{name: "CIDR covers a contained CIDR and IP", outer: restricted("10.0.0.0/8"), child: restricted("10.1.0.0/16", "10.2.3.4"), want: true},
		{name: "CIDR does not cover a wider CIDR", outer: restricted("10.1.0.0/16"), child: restricted("10.0.0.0/8"), want: false},
		{name: "a mapped IPv4 prefix reaches nothing, so cannot widen", outer: restricted("8.0.0.0/8"), child: restricted("::ffff:8.0.0.0/101"), want: true},
		{name: "domain does not cover an IP", outer: restricted("github.com"), child: restricted("140.82.112.3"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Covers(tt.outer, tt.child); got != tt.want {
				t.Fatalf("Covers(%+v, %+v) = %v, want %v", tt.outer, tt.child, got, tt.want)
			}
		})
	}
}

func TestMergePassthrough(t *testing.T) {
	got := Merge(
		Policy{Passthrough: []string{"Pinned.Vendor.com"}},
		Policy{Passthrough: []string{"*.corp.example", "pinned.vendor.com"}, DenyByDefault: true},
		Policy{},
	)
	if want := []string{"*.corp.example", "pinned.vendor.com"}; !slices.Equal(got.Passthrough, want) {
		t.Fatalf("Merge().Passthrough = %v, want %v", got.Passthrough, want)
	}
	if got.Allow != nil {
		t.Fatalf("passthrough admitted hosts: Allow = %v", got.Allow)
	}
}

func TestValidatePassthrough(t *testing.T) {
	if err := (Policy{Passthrough: []string{"*.corp.example", "10.0.0.0/8"}}).Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if err := (Policy{Passthrough: []string{"corp.example:443"}}).Validate(); err == nil {
		t.Fatal("Validate() accepted a passthrough entry with a port")
	}
}

func TestMatcherMatch(t *testing.T) {
	m := NewMatcher([]string{"*.corp.example", "10.0.0.0/8", "1.2.3.4"})
	for host, want := range map[string]string{
		"api.corp.example:443": "*.corp.example",
		"10.2.3.4:5432":        "10.0.0.0/8",
		"1.2.3.4":              "1.2.3.4",
	} {
		if got, ok := m.Match(host); !ok || got != want {
			t.Errorf("Match(%q) = %q, %v; want %q", host, got, ok, want)
		}
	}
	if got, ok := m.Match("corp.example"); ok {
		t.Errorf("Match(corp.example) = %q, want no match", got)
	}
}

func TestCoversPassthrough(t *testing.T) {
	tests := []struct {
		name         string
		outer, child Policy
		want         bool
	}{
		{name: "an empty child is covered by anything", outer: Policy{}, child: Policy{}, want: true},
		{name: "unrestricted outer without passthrough does not cover a child with it", outer: Policy{}, child: Policy{Passthrough: []string{"foo.example"}}, want: false},
		{name: "a subset is covered", outer: Policy{Passthrough: []string{"foo.example", "bar.example"}}, child: Policy{Passthrough: []string{"foo.example"}}, want: true},
		{name: "a wildcard covers a name beneath it", outer: Policy{Passthrough: []string{"*.corp.example"}}, child: Policy{Passthrough: []string{"db.corp.example"}}, want: true},
		{name: "a wider child is not", outer: Policy{Passthrough: []string{"db.corp.example"}}, child: Policy{Passthrough: []string{"*.corp.example"}}, want: false},
		{name: "held to it under restriction too", outer: Policy{DenyByDefault: true, Allow: []string{"*"}}, child: Policy{DenyByDefault: true, Passthrough: []string{"foo.example"}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Covers(tt.outer, tt.child); got != tt.want {
				t.Fatalf("Covers(%+v, %+v) = %v, want %v", tt.outer, tt.child, got, tt.want)
			}
		})
	}
}

func TestEffectiveInheritsPassthroughOnlyForAPersonsBox(t *testing.T) {
	project := Policy{Passthrough: []string{"pinned.example"}, Allow: []string{"github.com"}, DenyByDefault: true}
	own := Policy{Passthrough: []string{"own.example"}}
	person := Effective(own, false, project)
	if want := []string{"own.example", "pinned.example"}; !slices.Equal(person.Passthrough, want) {
		t.Fatalf("a person's box passes through %v, want %v", person.Passthrough, want)
	}
	discobox := Effective(own, true, project)
	if want := []string{"own.example"}; !slices.Equal(discobox.Passthrough, want) {
		t.Fatalf("a discobox's box passes through %v, want only its own %v", discobox.Passthrough, want)
	}
	if !discobox.DenyByDefault || !slices.Equal(discobox.Allow, []string{"github.com"}) {
		t.Fatalf("a discobox's box lost the inherited allow or restriction: %+v", discobox)
	}
	if got := Provision(project); len(got.Passthrough) != 0 || !got.DenyByDefault {
		t.Fatalf("Provision() = %+v, want restricted with nothing passed through", got)
	}
}
