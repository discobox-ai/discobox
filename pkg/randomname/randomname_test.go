package randomname

import (
	"bytes"
	"errors"
	"regexp"
	"testing"
)

func TestGenerateUsesDockerStyleFormat(t *testing.T) {
	name, err := generate(bytes.NewReader([]byte{0, 0}))
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if want := adjectives[0] + "_" + names[0]; name != want {
		t.Fatalf("generate = %q, want %q", name, want)
	}
}

func TestVocabularyIsSafeAndUnique(t *testing.T) {
	wordPattern := regexp.MustCompile(`^[a-z]+$`)
	for label, words := range map[string][]string{
		"adjective": adjectives[:],
		"name":      names[:],
	} {
		seen := make(map[string]struct{}, len(words))
		for _, word := range words {
			if !wordPattern.MatchString(word) {
				t.Errorf("%s %q contains characters outside a-z", label, word)
			}
			if _, ok := seen[word]; ok {
				t.Errorf("duplicate %s %q", label, word)
			}
			seen[word] = struct{}{}
		}
	}
}

func TestGeneratePropagatesRandomSourceError(t *testing.T) {
	want := errors.New("random source unavailable")
	_, err := generate(errorReader{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("generate error = %v, want wrapped %v", err, want)
	}
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}
