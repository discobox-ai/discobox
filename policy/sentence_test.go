package policy

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// sentences are the issue's and the ADR's examples, and every shape in this
// package's tests, each with the statement it reads as. Each is canonical.
var sentences = []struct {
	sentence string
	want     Statement
}{
	{`allow push on branch "discobox/issue-46" in repository $primary.repo`, Statement{
		Effect: Allow, Actions: []string{"push"}, ResourceType: "branch", Name: ptr("discobox/issue-46"),
		Scope: Scope{Type: "repository", Name: VarPrimaryRepo},
	}},
	{`allow create, update on pull request in repository $primary.repo when "its base is main"`, Statement{
		Effect: Allow, Actions: []string{ActionCreate, ActionUpdate}, ResourceType: "pull request",
		Scope: Scope{Type: "repository", Name: VarPrimaryRepo}, When: ptr("its base is main"),
	}},
	{`allow read on issue comment within issue "43" in repository $primary.repo`, Statement{
		Effect: Allow, Actions: []string{ActionRead}, ResourceType: "issue comment",
		Within: []Resource{{Type: "issue", Name: ptr("43")}},
		Scope:  Scope{Type: "repository", Name: VarPrimaryRepo},
	}},
	{`deny delete on anything in anywhere`, Statement{
		Effect: Deny, Actions: []string{ActionDelete}, ResourceType: Anything, Scope: Scope{Type: Anywhere},
	}},
	{`allow create, update on pull request in repository $primary.repo when "its head is discobox/issue-46 and its base is main"`, Statement{
		Effect: Allow, Actions: []string{ActionCreate, ActionUpdate}, ResourceType: "pull request",
		Scope: Scope{Type: "repository", Name: VarPrimaryRepo},
		When:  ptr("its head is discobox/issue-46 and its base is main"),
	}},
	{`allow create on issue in repository "acme/*"`, Statement{
		Effect: Allow, Actions: []string{ActionCreate}, ResourceType: "issue", Scope: Scope{Type: "repository", Name: "acme/*"},
	}},
	{`allow read on issue "{N}" in repository $primary.repo`, Statement{
		Effect: Allow, Actions: []string{ActionRead}, ResourceType: "issue", Name: ptr("{N}"),
		Scope: Scope{Type: "repository", Name: VarPrimaryRepo},
	}},
	{`allow manage on deployment in namespace "web" of cluster "staging"`, Statement{
		Effect: Allow, Actions: []string{ActionManage}, ResourceType: "deployment",
		Scope: Scope{Type: "namespace", Name: "web", Within: []Resource{{Type: "cluster", Name: ptr("staging")}}},
	}},
	{`allow update, delete on issue comment created by $this within issue within project $sources.docs.repo in organization "acme" of enterprise of host "github.com" unless "it is locked"`, Statement{
		Effect: Allow, Actions: []string{ActionUpdate, ActionDelete}, ResourceType: "issue comment", CreatedBy: VarThis,
		Within: []Resource{{Type: "project", Name: ptr("$sources.docs.repo")}, {Type: "issue"}},
		Scope: Scope{Type: "organization", Name: "acme", Within: []Resource{
			{Type: "host", Name: ptr("github.com")}, {Type: "enterprise"},
		}},
		Unless: ptr("it is locked"),
	}},
	{`deny force-push on branch in anywhere when "a \"quoted\" word" unless "a \\ backslash"`, Statement{
		Effect: Deny, Actions: []string{"force-push"}, ResourceType: "branch", Scope: Scope{Type: Anywhere},
		When: ptr(`a "quoted" word`), Unless: ptr(`a \ backslash`),
	}},
	{`allow update on branch $primary.branch created by $this in repository $primary.upstream`, Statement{
		Effect: Allow, Actions: []string{ActionUpdate}, ResourceType: "branch", Name: ptr(VarPrimaryBranch), CreatedBy: VarThis,
		Scope: Scope{Type: "repository", Name: VarPrimaryUpstream},
	}},
	{`allow read on file "a name, with \"in\" and on" in repository "acme/web"`, Statement{
		Effect: Allow, Actions: []string{ActionRead}, ResourceType: "file", Name: ptr(`a name, with "in" and on`),
		Scope: Scope{Type: "repository", Name: "acme/web"},
	}},
}

func TestParseReadsEachShape(t *testing.T) {
	for _, tc := range sentences {
		got, err := Parse(tc.sentence)
		if err != nil {
			t.Errorf("Parse(%s): %v", tc.sentence, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Parse(%s):\n got %#v\nwant %#v", tc.sentence, got, tc.want)
		}
		if f := Format(tc.want); f != tc.sentence {
			t.Errorf("Format:\n got %s\nwant %s", f, tc.sentence)
		}
	}
}

func TestEveryStatementInThisPackagesTestsRoundTrips(t *testing.T) {
	statements := pullRequests().Statements
	for _, tc := range sentences {
		statements = append(statements, tc.want)
	}
	got, err := ParseStatements(FormatStatements(statements))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, statements) {
		t.Fatalf("round trip:\n got %#v\nwant %#v", got, statements)
	}
}

func TestFormatIsCanonical(t *testing.T) {
	for _, tc := range []struct{ written, canonical string }{
		{"  allow\tread ,update  on\npull   request in repository \"$primary.repo\"  ",
			`allow read, update on pull request in repository $primary.repo`},
		{`deny delete on branch in anywhere unless "x" when "y"`,
			`deny delete on branch in anywhere when "y" unless "x"`},
		{`allow read on issue "ét\x65" in repository "a"`,
			`allow read on issue "\u00e9te" in repository "a"`},
		{`allow read on issue "$" in repository "a"`,
			`allow read on issue "$" in repository "a"`},
		{`allow read on issue $primary.nope in repository "$this"`,
			`allow read on issue "$primary.nope" in repository $this`},
	} {
		s, err := Parse(tc.written)
		if err != nil {
			t.Errorf("Parse(%s): %v", tc.written, err)
			continue
		}
		if got := Format(s); got != tc.canonical {
			t.Errorf("Format(Parse(%s)):\n got %s\nwant %s", tc.written, got, tc.canonical)
		}
	}
}

// TestParseFormatRoundTripsGeneratedStatements is the property: for every
// statement Validate accepts, Parse(Format(s)) is s, and formatting it again
// changes nothing.
func TestParseFormatRoundTripsGeneratedStatements(t *testing.T) {
	r := rand.New(rand.NewPCG(295, 1)) //nolint:gosec // G404: a fixed seed makes the generated statements reproducible; nothing here is secret.
	for range 5000 {
		s := randomStatement(r)
		p := Policy{Statements: []Statement{s}, Definitions: map[string]string{}}
		for _, w := range usedWords(s) {
			if !isReserved(w) {
				p.Definitions[w] = "A word."
			}
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("the generator made an invalid statement %#v: %v", s, err)
		}
		sentence := Format(s)
		got, err := Parse(sentence)
		if err != nil {
			t.Fatalf("Parse(%s): %v", sentence, err)
		}
		if !reflect.DeepEqual(got, s) {
			t.Fatalf("Parse(%s):\n got %#v\nwant %#v", sentence, got, s)
		}
		if again := Format(got); again != sentence {
			t.Fatalf("Format is not stable:\n got %s\nwant %s", again, sentence)
		}
	}
}

func usedWords(s Statement) []string {
	words := append([]string{s.ResourceType, s.Scope.Type}, s.Actions...)
	for _, r := range append(slicesClone(s.Within), s.Scope.Within...) {
		words = append(words, r.Type)
	}
	return words
}

func slicesClone(r []Resource) []Resource { return append([]Resource(nil), r...) }

var (
	genWords = []string{"read", "create", "update", "delete", "manage", "push", "force-push", "merge",
		"pull", "request", "issue", "comment", "branch", "repository", "namespace", "cluster", "workflow",
		"run", "by", "allow", "deny", "anywhere", "anything", "a/b", "x.y", "{N}", "*", "100%"}
	genNames = []string{"discobox/issue-46", "main", "43", "acme/*", "a b", "with, comma", `a "quote"`,
		`back\slash`, "in", "on", "when", "ünïcödé", "[a-z]*", "tab nbsp"}
	genVariables = []string{VarThis, VarPrimaryRepo, VarPrimaryHost, VarPrimaryBranch, VarPrimaryUpstream,
		VarPrimaryDefaultBranch, SourceRepoVar("docs"), SourceRepoVar("a-b_c")}
	genConditions = []string{"its base is main", `its title says "fix"`, `a \ and a \n`, "", "ünï, on, in",
		"when unless", "  spaced  "}
)

func randomWord(r *rand.Rand) string {
	words := make([]string, 1+r.IntN(3))
	for i := range words {
		words[i] = genWords[r.IntN(len(genWords))]
	}
	return strings.Join(words, " ")
}

// randomType is a word that is neither anything nor anywhere, which no slot
// but the resource and a deny's scope takes.
func randomType(r *rand.Rand) string {
	for {
		if w := randomWord(r); w != Anything && w != Anywhere {
			return w
		}
	}
}

func randomName(r *rand.Rand) string {
	if r.IntN(2) == 0 {
		return genVariables[r.IntN(len(genVariables))]
	}
	return genNames[r.IntN(len(genNames))]
}

func randomResource(r *rand.Rand) Resource {
	res := Resource{Type: randomType(r)}
	if r.IntN(2) == 0 {
		res.Name = ptr(randomName(r))
	}
	if r.IntN(4) == 0 {
		res.CreatedBy = VarThis
	}
	return res
}

func randomCondition(r *rand.Rand) *string {
	if r.IntN(2) == 0 {
		return nil
	}
	c := genConditions[r.IntN(len(genConditions))]
	if strings.TrimSpace(c) == "" {
		c = "x"
	}
	return ptr(c)
}

func randomStatement(r *rand.Rand) Statement {
	s := Statement{Effect: Allow, ResourceType: randomWord(r)}
	if s.ResourceType == Anywhere {
		s.ResourceType = Anything
	}
	if r.IntN(2) == 0 {
		s.Effect = Deny
	}
	seen := map[string]bool{}
	for range 1 + r.IntN(4) {
		if a := randomWord(r); !seen[a] && a != Anything && a != Anywhere {
			seen[a] = true
			s.Actions = append(s.Actions, a)
		}
	}
	if len(s.Actions) == 0 {
		s.Actions = []string{ActionRead}
	}
	head := randomResource(r)
	s.Name, s.CreatedBy = head.Name, head.CreatedBy
	for range r.IntN(3) {
		s.Within = append(s.Within, randomResource(r))
	}
	if s.Effect == Deny && r.IntN(3) == 0 {
		s.Scope = Scope{Type: Anywhere}
	} else {
		s.Scope = Scope{Type: randomType(r), Name: randomName(r)}
		for range r.IntN(3) {
			s.Scope.Within = append(s.Scope.Within, randomResource(r))
		}
	}
	s.When, s.Unless = randomCondition(r), randomCondition(r)
	return s
}

// FuzzFormatIsCanonical holds that whatever parses formats to a sentence that
// parses back to the same statement.
func FuzzFormatIsCanonical(f *testing.F) {
	for _, tc := range sentences {
		f.Add(tc.sentence)
	}
	f.Add(`allow read on "x"`)
	f.Add(`allow ,`)
	f.Fuzz(func(t *testing.T, sentence string) {
		s, err := Parse(sentence)
		if err != nil {
			return
		}
		canonical := Format(s)
		again, err := Parse(canonical)
		if err != nil {
			t.Fatalf("Parse(Format(Parse(%q))) = Parse(%q): %v", sentence, canonical, err)
		}
		if !reflect.DeepEqual(again, s) {
			t.Fatalf("Parse(%q):\n got %#v\nwant %#v", canonical, again, s)
		}
	})
}

func TestParseTeachesWhatItExpected(t *testing.T) {
	for _, tc := range []struct{ sentence, want string }{
		{``, `at the end of the sentence (byte 0): expected "allow" or "deny" to start the statement`},
		{`Allow read on issue in repository "a"`, `at "Allow" (byte 0): expected "allow" or "deny"`},
		{`allow on issue`, `at "on" (byte 6): expected an action`},
		{`allow push branch "x" in repository "a"`, `at the name or condition "x" (byte 18): expected "," and another action, or "on" and the resource type`},
		{`allow read, on issue`, `at "on" (byte 12): expected an action`},
		{`allow read on in repository "a"`, `at "in" (byte 14): expected the resource type`},
		{`allow read on issue in "a"`, `at the name or condition "a" (byte 23): expected the scope's type`},
		{`allow read on issue in repository "a" within issue`, `at "within" (byte 38): expected "of" and what the scope is in, "when" and a condition, "unless" and a condition, or the end of the sentence`},
		{`allow read on issue when "x" in repository "a"`, `at "in" (byte 29): expected "unless" and a condition, or the end of the sentence`},
		{`allow read on issue in repository "a" when its base is main`, `at "its" (byte 43): expected the condition after "when", in quotes`},
		{`allow read on issue in repository "a" when "x" when "y"`, `at "when" (byte 47): expected "unless" and a condition, or the end of the sentence`},
		{`allow read on issue created $this in repository "a"`, `at the variable $this (byte 28): expected "by" after "created"`},
		{`allow read on issue in repository "a`, `at the end of the sentence (byte 36): expected a closing " for the quote opened at byte 34`},
		{`allow read on issue in repository "a\q"`, `expected a quoted string whose backslashes are escapes`},
	} {
		_, err := Parse(tc.sentence)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), Grammar) {
			t.Errorf("Parse(%s) = %v\nwant it to say %q and show the grammar", tc.sentence, err, tc.want)
		}
	}
}

func TestParseStatementsNamesEachSentenceThatFails(t *testing.T) {
	_, err := ParseStatements([]string{`allow read on issue in repository "a"`, `allow`, `deny`})
	if err == nil || !strings.Contains(err.Error(), "statement 2: at the end") || !strings.Contains(err.Error(), "statement 3: at the end") {
		t.Fatalf("ParseStatements = %v", err)
	}
}

// An unquoted name reads as part of the type, and Validate says what is
// wrong, in every slot a name follows a type.
func TestAnUnquotedNameIsTaughtByValidate(t *testing.T) {
	for _, tc := range []struct{ sentence, want string }{
		{`allow read on issue in repository acme/web`, `scope "repository acme/web" has no name; a name is quoted or a variable`},
		{`allow push on branch main in repository $primary.repo`, `"branch main" is not a default word and has no definition; if its last word is a name, quote it`},
		{`allow read on issue comment within issue 43 in repository $primary.repo`, `"issue 43" is not a default word and has no definition; if its last word is a name, quote it`},
	} {
		s, err := Parse(tc.sentence)
		if err != nil {
			t.Fatal(err)
		}
		defs := map[string]string{"issue": "x", "issue comment": "x", "repository": "x", "repository acme/web": "x"}
		if err := (Policy{Statements: []Statement{s}, Definitions: defs}).Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%s) = %v, want it to say %q", tc.sentence, err, tc.want)
		}
	}
}

func TestASyntaxErrorEscapesWhatTheSentenceHeld(t *testing.T) {
	_, err := Parse("allow read on issue in repository \"a\" \"\x1b[2K\"")
	if err == nil || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `"\x1b[2K"`) {
		t.Fatalf("Parse = %q, want the escape shown escaped", err)
	}
	_, err = Parse("allow $a\x1b[2K on issue in repository \"a\"")
	if err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("Parse = %q, want the escape shown escaped", err)
	}
}

func TestValidateRefusesWordsASentenceCannotHold(t *testing.T) {
	for _, tc := range []struct{ word, want string }{
		{"pull  request", "two spaces in a row"},
		{"pull,request", "a comma"},
		{`pull "request"`, "has a quote"},
		{"repository \u201cacme/web\u201d of", "outside printable ASCII"},
		{"pull\u00a0request", "outside printable ASCII"},
		{"pull \u2066request\u2069", "outside printable ASCII"},
		{"repository\u2800$primary.repo\u2800of", "outside printable ASCII"},
		{"pull\x9b8Drequest", "outside printable ASCII"},
		{"repository x$primary.repo", "has a $"},
		{"pull $request", "has a $"},
		{"issue in review", `the word "in", which a sentence reads as a keyword`},
		{"created", `the word "created"`},
		{" pull request", "has space around it"},
		{"pull\trequest", "has a control character"},
	} {
		for _, slot := range []func(*Policy){
			func(p *Policy) { p.Statements[0].ResourceType = tc.word },
			func(p *Policy) { p.Statements[0].Actions = []string{tc.word} },
			func(p *Policy) { p.Statements[0].Scope.Type = tc.word },
			func(p *Policy) { p.Statements[0].Within = []Resource{{Type: tc.word}} },
		} {
			p := pullRequests()
			slot(&p)
			p.Definitions[tc.word] = "x"
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate with %q = %v, want it to say %q", tc.word, err, tc.want)
			} else if strings.Contains(err.Error(), strconv.Quote(tc.word)+" is defined but no statement uses it") {
				t.Errorf("Validate with %q = %v, want the refused word's definition not called unused", tc.word, err)
			}
			delete(p.Definitions, tc.word)
			if err := p.Validate(); err == nil || strings.Contains(err.Error(), strconv.Quote(tc.word)+" is not a default word and has no definition") {
				t.Errorf("Validate with %q undefined = %v, want it refused without asking for a definition", tc.word, err)
			}
		}
	}
}

// Quoted text is written in ASCII, so nothing in a condition can pass for its
// closing quote and seem to be followed by more.
func TestFormatWritesQuotedTextInASCII(t *testing.T) {
	s := Statement{
		Effect: Allow, Actions: []string{ActionRead}, ResourceType: "issue", Name: ptr("ünï\u2800x"),
		Scope: Scope{Type: "repository", Name: "a\xffb"},
		When:  ptr("a\u201d unless \u201cb\u2033 \u3164\u200b \u02ee\u05f4\u02f6 \u1425 \u030e \U00013441 日本 e\u0301"),
	}
	want := `allow read on issue "\u00fcn\u00ef\u2800x" in repository "a\xffb" when "a\u201d unless \u201cb\u2033 \u3164\u200b \u02ee\u05f4\u02f6 \u1425 \u030e \U00013441 \u65e5\u672c e\u0301"`
	if got := Format(s); got != want {
		t.Fatalf("Format:\n got %s\nwant %s", got, want)
	}
	got, err := Parse(want)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("Parse:\n got %#v\nwant %#v", got, s)
	}
}

// Tokens are Format's sentence, each part named for what it is, so a reader
// can highlight a sentence without parsing it again: Join(Tokens(s)) is
// Format(s).
func TestTokensNameEachPartOfTheSentence(t *testing.T) {
	s, err := Parse(`allow create, update on pull request created by $this within issue "43" in repository $primary.repo of org "acme" when "its base is main" unless "it is a draft"`)
	if err != nil {
		t.Fatal(err)
	}
	tokens := Tokens(s)
	if got := Join(tokens); got != Format(s) {
		t.Fatalf("Join(Tokens) = %q, Format = %q", got, Format(s))
	}
	var got []string
	for _, tok := range tokens {
		got = append(got, fmt.Sprintf("%d:%s", tok.Kind, tok.Text))
	}
	want := []string{
		fmt.Sprintf("%d:allow", TokenEffect), fmt.Sprintf("%d:create", TokenAction), fmt.Sprintf("%d:,", TokenPunct), fmt.Sprintf("%d:update", TokenAction),
		fmt.Sprintf("%d:on", TokenKeyword), fmt.Sprintf("%d:pull request", TokenType), fmt.Sprintf("%d:created by", TokenKeyword), fmt.Sprintf("%d:$this", TokenVariable),
		fmt.Sprintf("%d:within", TokenKeyword), fmt.Sprintf("%d:issue", TokenType), fmt.Sprintf("%d:\"43\"", TokenName),
		fmt.Sprintf("%d:in", TokenKeyword), fmt.Sprintf("%d:repository", TokenType), fmt.Sprintf("%d:$primary.repo", TokenVariable),
		fmt.Sprintf("%d:of", TokenKeyword), fmt.Sprintf("%d:org", TokenType), fmt.Sprintf("%d:\"acme\"", TokenName),
		fmt.Sprintf("%d:when", TokenKeyword), fmt.Sprintf("%d:\"its base is main\"", TokenCondition),
		fmt.Sprintf("%d:unless", TokenKeyword), fmt.Sprintf("%d:\"it is a draft\"", TokenCondition),
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("tokens:\n%s\nwant:\n%s", strings.Join(got, " | "), strings.Join(want, " | "))
	}
	if !tokens[2].Glued || tokens[3].Glued {
		t.Fatalf("the comma is glued to the action before it, and only it: %+v", tokens[2:4])
	}
}
