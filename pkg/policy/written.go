package policy

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Written is the policy its statements, each a sentence, and the definitions
// of their words say, parsed and validated: what an agent asks for with
// discobox-access, and what a person writes an approval rule with. It is nil
// when there are no statements. A sentence that does not parse is refused
// with what it reads as, and a policy that does not validate with Grammar, so
// whoever wrote it can write it again before anything is sent.
func Written(statements []string, definitions map[string]string) (*Policy, error) {
	if len(statements) == 0 {
		if len(definitions) > 0 {
			return nil, errors.New("definitions give the words of a policy's statements their meaning, and it has no statements; add them, or leave the definitions out")
		}
		return nil, nil
	}
	parsed, err := ParseStatements(statements)
	if err != nil {
		// Every SyntaxError already says what a statement reads as.
		return nil, fmt.Errorf("a statement does not parse: %w", err)
	}
	p := Policy{Statements: parsed, Definitions: definitions}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("the policy is not one a person could approve: %w\na statement reads: %s", err, Grammar)
	}
	return &p, nil
}

// Definitions is a command line's --define WORD=DEFINITION, repeatable, one
// word each: a flag value for the standard library's flag package and for
// pflag alike.
type Definitions map[string]string

func (d Definitions) String() string {
	return strings.Join(slices.Sorted(maps.Keys(d)), ", ")
}

func (d Definitions) Set(value string) error {
	word, definition, ok := strings.Cut(value, "=")
	word = strings.TrimSpace(word)
	if !ok || word == "" {
		return fmt.Errorf("%q is not WORD=DEFINITION, as push=\"update a branch's commits\"", value)
	}
	if _, seen := d[word]; seen {
		return fmt.Errorf("%q is defined twice", word)
	}
	d[word] = definition
	return nil
}

func (Definitions) Type() string { return "WORD=DEFINITION" }
