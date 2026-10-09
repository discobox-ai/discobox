package sandboxconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSkillsValidate_AcceptsASkillWithFiles(t *testing.T) {
	skills := Skills{"foo": {Skill: "# foo", Files: []SkillFile{
		{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\n"), Executable: true},
		{Path: "reference/notes.md", Content: []byte("notes")},
	}}}
	if err := skills.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestSkillsValidate_Refuses(t *testing.T) {
	file := func(p string) Skills {
		return Skills{"foo": {Skill: "# foo", Files: []SkillFile{{Path: p}}}}
	}
	cases := map[string]Skills{
		"empty name":           {"": {Skill: "x"}},
		"name with slash":      {"a/b": {Skill: "x"}},
		"name with backslash":  {`a\b`: {Skill: "x"}},
		"dot name":             {".": {Skill: "x"}},
		"dot-dot name":         {"..": {Skill: "x"}},
		"hidden name":          {".foo": {Skill: "x"}},
		"empty SKILL.md":       {"foo": {Skill: " \n"}},
		"empty path":           file(""),
		"absolute path":        file("/etc/passwd"),
		"escaping path":        file("../bar/SKILL.md"),
		"inner dot-dot":        file("a/../../b"),
		"unclean path":         file("./a"),
		"trailing slash":       file("a/"),
		"backslash path":       file(`a\b`),
		"SKILL.md as a file":   file(SkillFileName),
		"dot path":             file("."),
		"duplicate path":       {"foo": {Skill: "x", Files: []SkillFile{{Path: "a"}, {Path: "a"}}}},
		"file is a directory":  {"foo": {Skill: "x", Files: []SkillFile{{Path: "a"}, {Path: "a/b"}}}},
		"over the size limit":  {"foo": {Skill: strings.Repeat("x", MaxSkillsBytes+1)}},
		"over the limit, sums": {"a": {Skill: strings.Repeat("x", MaxSkillsBytes/2+1)}, "b": {Skill: strings.Repeat("x", MaxSkillsBytes/2)}},
		"paths over the limit": {"foo": {Skill: "x", Files: []SkillFile{{Path: strings.Repeat("a", MaxSkillsBytes)}}}},
		"too many files":       manyFiles(MaxSkillFiles + 1),
		"windows character":    {"a:b": {Skill: "x"}},
		"windows path char":    file("a/b?.md"),
		"control character":    file("a\x01b"),
		"NUL in a name":        {"a\x00b": {Skill: "x"}},
		"trailing dot":         {"foo.": {Skill: "x"}},
		"trailing space":       file("a /b"),
		"device name":          {"con": {Skill: "x"}},
		"device path":          file("bin/NUL.txt"),
		"names differ in case": {"Foo": {Skill: "x"}, "foo": {Skill: "x"}},
		"paths differ in case": {"foo": {Skill: "x", Files: []SkillFile{{Path: "Bin"}, {Path: "bin/run.sh"}}}},
		"skill.md in any case": file("skill.md"),
		"under SKILL.md":       file("SKILL.md/notes.md"),
		"under skill.md":       file("skill.md/x"),
	}
	for name, skills := range cases {
		t.Run(name, func(t *testing.T) {
			if err := skills.Validate(); err == nil {
				t.Fatal("Validate() = nil, want a refusal")
			}
		})
	}
}

func TestSkillsSizeCountsEveryFile(t *testing.T) {
	skills := Skills{
		"a": {Skill: "12345", Files: []SkillFile{{Path: "x", Content: []byte("123")}}},
		"b": {Skill: "12"},
	}
	// 5 + 2 of SKILL.md, 3 of content, and 1 of the path "x".
	if got := skills.Size(); got != 11 {
		t.Fatalf("Size() = %d, want 11", got)
	}
	if got := skills.Names(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Names() = %v, want [a b]", got)
	}
}

// Content is bytes, so a skill's images and binaries survive the bootstrap's
// JSON as base64.
func TestSkillFileContentRoundTripsAsBase64(t *testing.T) {
	in := Skills{"foo": {Skill: "# foo", Files: []SkillFile{{Path: "logo.png", Content: []byte{0x89, 'P', 'N', 'G', 0, 0xff}}}}}
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"content":"iVBORwD/"`) {
		t.Fatalf("encoded = %s, want base64 content", encoded)
	}
	var out Skills
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestEffective_SkillsAreRuntimeOnlyAndCopied(t *testing.T) {
	doc := Document{Runtime: RuntimeLayer{Skills: Skills{"foo": {Skill: "# foo", Files: []SkillFile{{Path: "a", Content: []byte("a")}}}}}}
	cfg, _ := Effective(doc)
	if !reflect.DeepEqual(cfg.Skills, doc.Runtime.Skills) {
		t.Fatalf("Skills = %+v, want the runtime layer's", cfg.Skills)
	}
	cfg.Skills["foo"].Files[0].Content[0] = 'b'
	if doc.Runtime.Skills["foo"].Files[0].Content[0] != 'a' {
		t.Fatal("Effective's Skills share content with the document")
	}
	// Once in the bootstrap, not twice: provenance leaves them out.
	if _, prov := Effective(doc); prov.Runtime.Skills != nil {
		t.Fatalf("provenance skills = %+v, want none", prov.Runtime.Skills)
	}
	if cfg, _ := Effective(Document{}); cfg.Skills != nil {
		t.Fatalf("Skills = %+v, want nil when the runtime layer has none", cfg.Skills)
	}
}

func manyFiles(n int) Skills {
	files := make([]SkillFile, n)
	for i := range files {
		files[i].Path = fmt.Sprintf("f%d", i)
	}
	return Skills{"foo": {Skill: "x", Files: files}}
}

// Skills at exactly the file limit are taken, and names that differ in more
// than case are separate skills.
func TestSkillsValidate_AcceptsTheLimitsAndPortableNames(t *testing.T) {
	if err := manyFiles(MaxSkillFiles).Validate(); err != nil {
		t.Fatalf("%d files: %v", MaxSkillFiles, err)
	}
	skills := Skills{"foo-bar_1.2": {Skill: "x", Files: []SkillFile{{Path: ".env"}, {Path: "con-tents.md"}}}, "Foo2": {Skill: "x"}}
	if err := skills.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}
