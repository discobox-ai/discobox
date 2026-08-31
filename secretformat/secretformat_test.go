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

// A described value keeps the shape its provider is recognized by, and says
// nothing about where the credential belongs: the host is a binding somebody
// sets on the secret, never a guess from a prefix.
func TestProviderDescribeKeepsTheShapeAndClaimsNoHost(t *testing.T) {
	cases := map[string]string{
		"sk-ant-api03-" + strings.Repeat("a", 95):                     "sk-ant-",
		"sk-proj-" + strings.Repeat("b", 48):                          "sk-proj-",
		"github_pat_" + strings.Repeat("c", 82):                       "github_pat_",
		"xoxb-1111111111111-2222222222222-" + strings.Repeat("d", 24): "xoxb-",
	}
	for value, prefix := range cases {
		format := Describe(value)
		tmpl, err := Parse(format)
		if err != nil {
			t.Fatalf("format %q does not parse: %v", format, err)
		}
		if !strings.HasPrefix(format, prefix) {
			t.Fatalf("Describe(%q) = %q, want it to keep %q — an SDK that checks the prefix rejects a sentinel without it", value, format, prefix)
		}
		gen, err := tmpl.Generate()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.HasPrefix(gen, prefix) {
			t.Fatalf("generated %q does not wear %q", gen, prefix)
		}
		if !tmpl.Validate(gen) {
			t.Fatalf("generated %q does not satisfy its own template", gen)
		}
	}
}

// A value the table does not know is described structurally, and the template
// it produces must carry none of the value's own bytes past its scheme marker:
// the format is stored on the secret, and a credential that leaks into it is a
// credential leaked wherever that record goes.
func TestInferredFormatCarriesNoCredentialBytes(t *testing.T) {
	// A random tail that happens to contain an early separator is the case
	// that used to bake real bytes in: "AIzal5lf-" was kept as a literal.
	for _, value := range []string{
		"AIzal5lf-abcdefghijklmnopqrstuvwxyz012345",
		"ghp_ab-cdefghijklmnopqrstuvwxyz0123456789",
		"tok_secret-tail-here0123456789",
	} {
		format := Describe(value)
		tmpl, err := Parse(format)
		if err != nil {
			t.Fatalf("format %q does not parse: %v", format, err)
		}
		gen, err := tmpl.Generate()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		// Every literal in the template has to appear in the same place in a
		// freshly generated value; what must not survive is any run of the
		// original's entropy. Compare the two: past the scheme marker they may
		// not share a byte position.
		marker := schemeMarker(value)
		for i := len(marker); i < len(value) && i < len(gen); i++ {
			if value[i] == gen[i] && isLiteralAt(format, i) {
				t.Fatalf("Describe(%q) = %q keeps the value's own byte at %d", value, format, i)
			}
		}
	}
}

// schemeMarker is the leading word plus separator a template may legitimately
// keep, by the same rule the inference uses: wholly alphabetic and short.
func schemeMarker(value string) string {
	for i, r := range value {
		if r == '-' || r == '_' {
			word := value[:i]
			if word == "" || len(word) > 10 {
				return ""
			}
			for _, c := range word {
				if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
					return ""
				}
			}
			return value[:i+1]
		}
	}
	return ""
}

// isLiteralAt reports whether the template holds a literal at that offset of a
// generated value, which is the only way a byte can be carried over.
//
// Only the leading literal is walked: past the first {...} the offsets are set
// by a run whose length this cannot know, and everything there is generated by
// definition.
func isLiteralAt(format string, offset int) bool {
	head, _, _ := strings.Cut(format, "{")
	return offset < len(head)
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
