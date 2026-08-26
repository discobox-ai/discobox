package frontmatter

import (
	"reflect"
	"testing"
)

func TestParseCommentedBlockAfterShebang(t *testing.T) {
	parsed, err := Parse([]byte("#!/bin/bash\n#---\n# name: Discobox API\n# description: hot reload\n#---\nexec task dev\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !parsed.HasShebang {
		t.Error("HasShebang = false, want true")
	}
	if parsed.Delimiter != DelimiterHash {
		t.Errorf("Delimiter = %q, want %q", parsed.Delimiter, DelimiterHash)
	}
	if got, want := string(parsed.Meta), "name: Discobox API\ndescription: hot reload\n"; got != want {
		t.Errorf("Meta = %q, want %q", got, want)
	}
	if got, want := parsed.Body, "exec task dev\n"; got != want {
		t.Errorf("Body = %q, want %q", got, want)
	}
}

func TestParseDelimiterFormsAgree(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"plain", "---\nname: X\n---\nbody\n"},
		{"hash", "#---\n# name: X\n#---\nbody\n"},
		{"slash", "//---\n// name: X\n//---\nbody\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := Parse([]byte(tc.input))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			fields, err := Decode(parsed.Meta)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got := fields.String("name"); got != "X" {
				t.Errorf("name = %q, want X", got)
			}
			if parsed.Body != "body\n" {
				t.Errorf("Body = %q, want %q", parsed.Body, "body\n")
			}
		})
	}
}

// A closing delimiter has to match the opening one, so a plain `---` inside a
// `#---` block is content rather than the end of it.
func TestParseClosingDelimiterMustMatch(t *testing.T) {
	if _, err := Parse([]byte("#---\n# name: X\n---\n")); err == nil {
		t.Fatal("Parse succeeded, want missing closing delimiter")
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ name, input string }{
		{"empty", ""},
		{"no delimiter", "#!/bin/bash\necho hi\n"},
		{"unterminated", "---\nname: X\n"},
		{"shebang only", "#!/bin/bash\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.input)); err == nil {
				t.Fatal("Parse succeeded, want error")
			}
		})
	}
}

func TestParseNormalizesCRLF(t *testing.T) {
	parsed, err := Parse([]byte("#---\r\n# name: X\r\n#---\r\nbody\r\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := string(parsed.Meta), "name: X\n"; got != want {
		t.Errorf("Meta = %q, want %q", got, want)
	}
}

func TestDecodeNormalizesKeySpelling(t *testing.T) {
	fields, err := Decode([]byte("Run-As: root\nLANGUAGE_ID: go\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := fields.String("run_as"); got != "root" {
		t.Errorf("run_as = %q, want root", got)
	}
	if got := fields.String("language_id"); got != "go" {
		t.Errorf("language_id = %q, want go", got)
	}
}

func TestDecodeEmptyBlockIsNotAnError(t *testing.T) {
	fields, err := Decode(nil)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(fields) != 0 {
		t.Errorf("fields = %v, want empty", fields)
	}
	if got := fields.String("name"); got != "" {
		t.Errorf("name = %q, want empty", got)
	}
}

func TestFieldsAccessors(t *testing.T) {
	fields, err := Decode([]byte("blocking: yes\nquoted: \"true\"\noff: no\nignore: vendor/**\nexclude:\n  - dist/**\n  - \"\"\nextra: 7\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !fields.Bool("blocking") {
		t.Error("blocking = false, want true")
	}
	if !fields.Bool("quoted") {
		t.Error("quoted = false, want true")
	}
	if fields.Bool("off") {
		t.Error("off = true, want false")
	}
	if fields.Bool("missing") {
		t.Error("missing = true, want false")
	}
	if got, want := fields.Strings("ignore", "exclude"), []string{"vendor/**", "dist/**"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Strings = %v, want %v", got, want)
	}
	if got, want := fields.Except("blocking", "quoted", "off", "ignore", "exclude"), map[string]any{"extra": 7}; !reflect.DeepEqual(got, want) {
		t.Errorf("Except = %v, want %v", got, want)
	}
}

// String takes its keys most-canonical-first, so the canonical spelling wins
// over an alias when a file carries both.
func TestFieldsStringPrefersFirstPresentKey(t *testing.T) {
	fields, err := Decode([]byte("language_id: go\nlanguage: python\n"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := fields.String("language_id", "language"); got != "go" {
		t.Errorf("String = %q, want go", got)
	}
}

func TestNormalizeID(t *testing.T) {
	for _, tc := range []struct{ filename, want string }{
		{"10-discobox-api.sh", "discobox-api"},
		{"90-review-lint.sh", "review-lint"},
		{"go-lsp.sh", "go-lsp"},
		{"Some Hook.MD", "some-hook"},
		{"check-1.2", "check-1-2"},
		{"...", ""},
	} {
		if got := NormalizeID(tc.filename); got != tc.want {
			t.Errorf("NormalizeID(%q) = %q, want %q", tc.filename, got, tc.want)
		}
	}
}

func TestDefaultName(t *testing.T) {
	for _, tc := range []struct{ filename, want string }{
		{"10-discobox-api.sh", "Discobox Api"},
		{"go-lsp.sh", "Go Lsp"},
		{"...", ""},
	} {
		if got := DefaultName(tc.filename); got != tc.want {
			t.Errorf("DefaultName(%q) = %q, want %q", tc.filename, got, tc.want)
		}
	}
}

func TestHasShebangLine(t *testing.T) {
	if !HasShebangLine([]byte("#!/bin/sh\n")) {
		t.Error("HasShebangLine = false, want true")
	}
	if HasShebangLine([]byte("# not a shebang\n")) {
		t.Error("HasShebangLine = true, want false")
	}
}
