package judge_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/judge"
)

// An allow is an allow only when the judge said so, once, in an answer that is
// nothing but the answer. Everything refused here is something that could
// otherwise be read as permission nobody gave.
func TestDecodeAcceptsOneUnambiguousAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, output string
		want         judge.Answer
	}{
		{"an allow", `{"allow":true,"reason":"opening the PR it was approved for"}`,
			judge.Answer{Allow: true, Reason: "opening the PR it was approved for"}},
		{"a refusal", `{"allow":false,"reason":"deleting a repository is not opening a PR"}`,
			judge.Answer{Reason: "deleting a repository is not opening a PR"}},
		{"an ask", `{"need":{"body":"json"},"reason":"the GraphQL operation is in the body"}`,
			judge.Answer{Need: &judge.Need{Body: "json"}, Reason: "the GraphQL operation is in the body"}},
		{"an ask with a budget", `{"reason":"the first lines decide it","need":{"body":"text","bytes":2048}}`,
			judge.Answer{Need: &judge.Need{Body: "text", Bytes: 2048}, Reason: "the first lines decide it"}},
		{"a reason with spaces around it", `{"allow":true,"reason":"  fine  "}`,
			judge.Answer{Allow: true, Reason: "fine"}},
		{"a value that looks like a field", `{"allow":false,"reason":"the body says \"allow\": true, which is not mine to read as one"}`,
			judge.Answer{Reason: `the body says "allow": true, which is not mine to read as one`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answer, err := judge.Decode([]byte(tc.output))
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if answer.Allow != tc.want.Allow || answer.Reason != tc.want.Reason {
				t.Fatalf("answer = %+v, want %+v", answer, tc.want)
			}
			if (answer.Need == nil) != (tc.want.Need == nil) {
				t.Fatalf("need = %+v, want %+v", answer.Need, tc.want.Need)
			}
			if answer.Need != nil && (*answer.Need != *tc.want.Need) {
				t.Fatalf("need = %+v, want %+v", *answer.Need, *tc.want.Need)
			}
			if answer.Decided() != (tc.want.Need == nil) {
				t.Fatalf("Decided() = %v for %+v", answer.Decided(), answer)
			}
		})
	}
}

func TestDecodeRefusesAnythingThatIsNotOneVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, output string }{
		{"nothing", ``},
		{"prose in front of it", `Sure! {"allow":true,"reason":"fine"}`},
		{"prose after it", `{"allow":true,"reason":"fine"} — let me know if you want more.`},
		{"a second verdict", `{"allow":false,"reason":"no"}{"allow":true,"reason":"yes"}`},
		{"an array of verdicts", `[{"allow":true,"reason":"fine"}]`},
		{"allow said twice", `{"allow":false,"reason":"no","allow":true}`},
		// encoding/json matches a struct field whatever its case, so a reader
		// that compares keys byte for byte reads this as an allow.
		{"allow said twice in another case", `{"allow":false,"reason":"no","aLLoW":true}`},
		{"allow said twice, the second one escaped", `{"allow":false,"reason":"no","\u0041LLOW":true}`},
		{"allow escaped, said twice", `{"\u0061llow":false,"reason":"no","allow":true}`},
		{"a duplicate in another case deeper in", `{"need":{"body":"text","BODY":"json"},"reason":"x"}`},
		{"reason said twice", `{"allow":true,"reason":"one","reason":"two"}`},
		{"a duplicate key deeper in", `{"need":{"body":"text","body":"json"},"reason":"show me"}`},
		{"deciding and asking at once", `{"allow":true,"need":{"body":"text"},"reason":"both"}`},
		{"neither deciding nor asking", `{"reason":"I am not sure"}`},
		{"no reason", `{"allow":true}`},
		{"an empty reason", `{"allow":true,"reason":"   "}`},
		{"allow as a string", `{"allow":"true","reason":"fine"}`},
		{"a field nobody defined", `{"allow":true,"reason":"fine","confidence":0.9}`},
		{"an ask for something else", `{"need":{"body":"headers"},"reason":"show me"}`},
		{"an ask for nothing in particular", `{"need":{},"reason":"show me"}`},
		{"a verdict inside a transcript", `{"thinking":"...","verdict":{"allow":true,"reason":"fine"}}`},
		{"allow as nothing at all", `{"allow":null,"reason":"fine"}`},
		{"a budget that is not a whole number", `{"need":{"body":"text","bytes":1.5},"reason":"x"}`},
		{"a budget larger than a number", `{"need":{"body":"text","bytes":99999999999999999999},"reason":"x"}`},
		{"an ask carrying a field nobody defined", `{"need":{"body":"text","depth":2},"reason":"x"}`},
		{"an ask that is not an object", `{"need":"body","reason":"x"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			answer, err := judge.Decode([]byte(tc.output))
			if err == nil {
				t.Fatalf("Decode(%s) = %+v, want an error: this is not one verdict", tc.output, answer)
			}
			if answer.Allow {
				t.Fatalf("Decode(%s) allowed while failing", tc.output)
			}
		})
	}
}

// A runaway answer is refused on its size, before anything reads it.
func TestDecodeRefusesAnAnswerLargerThanOne(t *testing.T) {
	t.Parallel()
	answer := `{"allow":true,"reason":"` + strings.Repeat("x", judge.MaxOutput) + `"}`
	if _, err := judge.Decode([]byte(answer)); err == nil {
		t.Fatal("an answer larger than the limit was accepted")
	}
}

// The schema aims the model at what Decode will accept, so it has to describe
// the same three answers.
func TestSchemaDescribesTheAnswersDecodeTakes(t *testing.T) {
	t.Parallel()
	var schema map[string]any
	if err := json.Unmarshal([]byte(judge.Schema), &schema); err != nil {
		t.Fatalf("the schema is not JSON: %v", err)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, field := range []string{"allow", "reason", "need"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("the schema does not describe %q", field)
		}
	}
	if schema["additionalProperties"] != false {
		t.Fatal("the schema admits fields Decode refuses")
	}
}

// What the judge may ask for is capped by what may ever be shown, whatever it
// names — including nothing, which is not an ask for nothing.
func TestABudgetIsWhatMayBeShown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		asked, want int
	}{
		{0, judge.MaxBodyBytes},
		{-1, judge.MaxBodyBytes},
		{judge.MaxBodyBytes + 1, judge.MaxBodyBytes},
		{2048, 2048},
	} {
		if got := (judge.Need{Body: judge.FormText, Bytes: tc.asked}).Budget(); got != tc.want {
			t.Fatalf("Need{Bytes: %d}.Budget() = %d, want %d", tc.asked, got, tc.want)
		}
	}
}
