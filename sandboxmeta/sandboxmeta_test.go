package sandboxmeta

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    Meta
		wantErr string
	}{
		{name: "empty file is no meta", data: "", want: Meta{Tags: map[string]string{}}},
		{name: "whitespace is no meta", data: " \n\t", want: Meta{Tags: map[string]string{}}},
		{name: "empty object", data: "{}", want: Meta{Tags: map[string]string{}}},
		{
			name: "description and tags",
			data: `{"description": "Fixing the reaper\nand its test", "tags": {"wip": "", "ticket": "ENG-12"}}`,
			want: Meta{Description: "Fixing the reaper\nand its test", Tags: map[string]string{"wip": "", "ticket": "ENG-12"}},
		},
		{name: "a list is not meta", data: `["wip"]`, wantErr: "JSON object"},
		{name: "an unknown field is refused", data: `{"title": "x"}`, wantErr: "unknown field"},
		{name: "a non-string tag value is refused", data: `{"tags": {"count": 3}}`, wantErr: "JSON object"},
		{name: "two values are refused", data: `{} {}`, wantErr: "more than one"},
		{name: "one bad key refuses the file", data: `{"tags": {"ok": "", "a=b": ""}}`, wantErr: `cannot contain '='`},
		{name: "a line break in a tag value is refused", data: `{"tags": {"note": "a\nb"}}`, wantErr: "control characters"},
		{name: "a control character in the description is refused", data: `{"description": "a\u0007"}`, wantErr: "control characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.data))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse() error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseRefusesAnOversizedFile(t *testing.T) {
	if _, err := Parse(make([]byte, MaxFileSize+1)); err == nil {
		t.Fatal("Parse() of an oversized file succeeded")
	}
}

func TestEncodeRoundTrips(t *testing.T) {
	meta := Meta{Description: "what it is for", Tags: map[string]string{"zeta": "1", "alpha": ""}}
	data, err := Encode(meta)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"description\": \"what it is for\",\n  \"tags\": {\n    \"alpha\": \"\",\n    \"zeta\": \"1\"\n  }\n}\n"
	if string(data) != want {
		t.Fatalf("Encode() = %q, want %q", data, want)
	}
	back, err := Parse(data)
	if err != nil || !reflect.DeepEqual(back, meta) {
		t.Fatalf("Parse(Encode()) = %v, %v; want %v", back, err, meta)
	}
	if empty, _ := Encode(Meta{}); string(empty) != "{}\n" {
		t.Fatalf("Encode(Meta{}) = %q, want an empty object", empty)
	}
}

func TestValidateKey(t *testing.T) {
	for _, key := range []string{"wip", "team/area", "ticket.id", "日本"} {
		if err := ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
		}
	}
	for _, key := range []string{"", "a b", "a=b", "a,b", "tab\t", strings.Repeat("k", MaxKeyLength+1)} {
		if err := ValidateKey(key); err == nil {
			t.Errorf("ValidateKey(%q) = nil, want an error", key)
		}
	}
}

func TestValidateLimits(t *testing.T) {
	tags := map[string]string{}
	for i := range MaxTags + 1 {
		tags[strings.Repeat("k", i+1)] = ""
	}
	if err := ValidateTags(tags); err == nil {
		t.Fatal("ValidateTags() of too many tags succeeded")
	}
	if err := ValidateValue("k", strings.Repeat("v", MaxValueLength+1)); err == nil {
		t.Fatal("ValidateValue() of an oversized value succeeded")
	}
	if err := ValidateDescription(strings.Repeat("d", MaxDescriptionLength+1)); err == nil {
		t.Fatal("ValidateDescription() of an oversized description succeeded")
	}
	if err := ValidateValue("k", "ENG-1,ENG-2"); err == nil {
		t.Fatal("ValidateValue() of a value with a comma succeeded; a listing would read it as two tags")
	}
	if err := ValidateValue("k", "spaces and = are fine"); err != nil {
		t.Fatalf("ValidateValue() = %v, want nil", err)
	}
	if err := ValidateDescription("two\nlines\twith a tab"); err != nil {
		t.Fatalf("ValidateDescription() = %v, want nil", err)
	}
}

func TestApply(t *testing.T) {
	current := Meta{Description: "old", Tags: map[string]string{"wip": "", "ticket": "ENG-1"}}
	got, err := Apply(current, Change{SetTags: map[string]string{"ticket": "ENG-2", "owner": "sam"}, RemoveTags: []string{"wip", "absent"}})
	if err != nil {
		t.Fatal(err)
	}
	want := Meta{Description: "old", Tags: map[string]string{"ticket": "ENG-2", "owner": "sam"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Apply() = %v, want %v", got, want)
	}
	if _, ok := current.Tags["owner"]; ok || current.Tags["ticket"] != "ENG-1" {
		t.Fatalf("Apply() modified its input: %v", current)
	}

	description := "new"
	got, err = Apply(current, Change{Description: &description})
	if err != nil || got.Description != "new" || !reflect.DeepEqual(got.Tags, current.Tags) {
		t.Fatalf("Apply() of a description = %v, %v", got, err)
	}
	cleared := ""
	if got, _ = Apply(current, Change{Description: &cleared}); got.Description != "" {
		t.Fatalf("Apply() clearing the description = %q", got.Description)
	}

	for _, bad := range []Change{
		{SetTags: map[string]string{"wip": "x"}, RemoveTags: []string{"wip"}},
		{SetTags: map[string]string{"bad key": ""}},
		{RemoveTags: []string{"a=b"}},
	} {
		if _, err := Apply(current, bad); err == nil {
			t.Errorf("Apply(%+v) succeeded", bad)
		}
		if err := bad.Check(); err == nil {
			t.Errorf("Check(%+v) succeeded", bad)
		}
	}
}

func TestFormatting(t *testing.T) {
	if got := FormatTags(map[string]string{"wip": "", "ticket": "ENG-12"}); got != "ticket=ENG-12,wip" {
		t.Fatalf("FormatTags() = %q", got)
	}
	if got := FormatTags(nil); got != "" {
		t.Fatalf("FormatTags(nil) = %q, want empty", got)
	}
	if got := Summary("\n  Fixing the reaper  \nmore detail"); got != "Fixing the reaper" {
		t.Fatalf("Summary() = %q", got)
	}
}

func TestSelectors(t *testing.T) {
	tags := map[string]string{"wip": "", "ticket": "ENG-12"}
	tests := []struct {
		selector string
		want     bool
	}{
		{"wip", true},
		{"wip=", true},
		{"ticket", true},
		{"ticket=ENG-12", true},
		{"ticket=ENG-13", false},
		{"ticket=", false},
		{"owner", false},
	}
	for _, tt := range tests {
		selector, err := ParseSelector(tt.selector)
		if err != nil {
			t.Fatalf("ParseSelector(%q) = %v", tt.selector, err)
		}
		if got := selector.Matches(tags); got != tt.want {
			t.Errorf("%q.Matches() = %v, want %v", tt.selector, got, tt.want)
		}
		if selector.String() != tt.selector {
			t.Errorf("String() = %q, want %q", selector.String(), tt.selector)
		}
	}
	if _, err := ParseSelector("bad key"); err == nil {
		t.Fatal("ParseSelector() of an invalid key succeeded")
	}
	all, err := ParseSelectors([]string{"wip", "ticket=ENG-12"})
	if err != nil {
		t.Fatal(err)
	}
	if !MatchesAll(tags, all) || MatchesAll(map[string]string{"wip": ""}, all) || !MatchesAll(nil, nil) {
		t.Fatal("MatchesAll() did not require every selector")
	}
}
