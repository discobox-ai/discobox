package secretformat

import (
	"strings"
	"testing"
)

func TestParseErrors(t *testing.T) {
	for _, format := range []string{"", "   ", "sk-{unknown:4}", "sk-{hex:0}", "sk-{hex:}", "sk-{hex}"} {
		if _, err := Parse(format); err == nil {
			t.Fatalf("Parse(%q) expected error", format)
		}
	}
}

func TestGenerateMatchesValidate(t *testing.T) {
	formats := []string{
		"sk-ant-oat01-{base64url:93}",
		"ghp_{base62:36}",
		"AKIA{base32:16}",
		"{hex:64}",
		"xoxb-{digits:13}-{digits:13}-{base62:24}",
	}
	for _, format := range formats {
		tmpl, err := Parse(format)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", format, err)
		}
		for i := 0; i < 50; i++ {
			value, err := tmpl.Generate()
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			if !tmpl.Validate(value) {
				t.Fatalf("generated %q does not validate against %q", value, format)
			}
		}
	}
}

func TestGenerateIsRandom(t *testing.T) {
	tmpl, err := Parse("sk-{base64url:40}")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := tmpl.Generate()
	b, _ := tmpl.Generate()
	if a == b {
		t.Fatal("two generated values collided; not random")
	}
	if !strings.HasPrefix(a, "sk-") || !strings.HasPrefix(b, "sk-") {
		t.Fatalf("generated values missing literal prefix: %q %q", a, b)
	}
}

func TestValidateRejectsWrongShape(t *testing.T) {
	tmpl, err := Parse("ghp_{base62:8}")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ghp_short", "ghp_toolongvalue", "gho_abcd1234", "ghp_abc!1234"} {
		if tmpl.Validate(bad) {
			t.Fatalf("Validate(%q) = true, want false", bad)
		}
	}
	if !tmpl.Validate("ghp_abcd1234") {
		t.Fatal("Validate rejected a well-shaped value")
	}
}

func TestProviderDescribe(t *testing.T) {
	cases := map[string]struct{ host string }{
		"sk-ant-api03-" + strings.Repeat("a", 95):                     {host: "api.anthropic.com"},
		"sk-proj-" + strings.Repeat("b", 48):                          {host: "api.openai.com"},
		"ghp_" + strings.Repeat("c", 36):                              {host: "api.github.com"},
		"xoxb-1111111111111-2222222222222-" + strings.Repeat("d", 24): {host: "slack.com"},
	}
	for value, want := range cases {
		format, host := Describe(value)
		if host != want.host {
			t.Fatalf("Describe(%q) host = %q, want %q", value, host, want.host)
		}
		tmpl, err := Parse(format)
		if err != nil {
			t.Fatalf("provider format %q does not parse: %v", format, err)
		}
		gen, _ := tmpl.Generate()
		if p, ok := MatchProvider(gen); !ok || p.Host != want.host {
			t.Fatalf("generated %q did not round-trip to provider host %q", gen, want.host)
		}
	}
}

func TestInferRoundTrips(t *testing.T) {
	values := []string{
		"sk-mytool-abcDEF123456789",
		"tok_0123456789abcdef",
		"AKIAABCDEFGHIJKLMNOP",
		"deadbeefcafe0123",
		"header.payload.signature01",
		"1234567890",
	}
	for _, value := range values {
		tmpl := Infer(value)
		if !tmpl.Validate(value) {
			t.Fatalf("Infer(%q) => %q does not validate original", value, tmpl.String())
		}
	}
}

func TestInferDoesNotLeakEntropy(t *testing.T) {
	// A random body must not appear as a literal in the inferred template.
	value := "sk-Zx9Qw7Rt2Yu4Pl6"
	tmpl := Infer(value)
	format := tmpl.String()
	if !strings.HasPrefix(format, "sk-{") {
		t.Fatalf("Infer(%q) = %q, want only the low-entropy prefix kept literal", value, format)
	}
	// The generated sentinel should share only the literal prefix, not the body.
	gen, _ := tmpl.Generate()
	if gen == value {
		t.Fatal("generated sentinel equals the original value")
	}
	if !strings.HasPrefix(gen, "sk-") {
		t.Fatalf("generated %q missing prefix", gen)
	}
}

func TestClassifyTightest(t *testing.T) {
	cases := map[string]string{
		"0123":     "digits",
		"abcdef01": "hex",
		"ABCDEF01": "HEX",
		"abcxyz":   "lower",
		"ABCXYZ":   "upper",
		"aB3xY9":   "alnum",
		"aB3-_x":   "base64url",
		"aB3+/x":   "base64",
	}
	for seg, want := range cases {
		if got := classify(seg); got != want {
			t.Fatalf("classify(%q) = %q, want %q", seg, got, want)
		}
	}
}
