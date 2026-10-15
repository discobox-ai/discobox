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
