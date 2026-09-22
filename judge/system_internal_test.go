package judge

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// A stored verdict names the prompt version that produced it, which is only
// worth storing while that version names one set of words.
func TestThePromptVersionNamesTheseWords(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte(System))
	if got := hex.EncodeToString(sum[:]); got != systemDigest {
		t.Fatalf("the system prompt now digests to %s: bump PromptVersion and pin the new words in systemDigest", got)
	}
}
