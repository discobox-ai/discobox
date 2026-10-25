package services

import (
	"encoding/json"
	"testing"
)

func TestCachedHarnessSelectionPreservesPostedHistory(t *testing.T) {
	raw := json.RawMessage(`{"sessions":[{"terminalId":"ex_1","harnessId":"old","currentHarnessId":"old","future":42}],"futureTop":true}`)
	selected, err := sandboxAgentStatusCurrentHarness(raw, "new")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sessions []struct {
			HarnessID string `json:"harnessId"`
			Current   string `json:"currentHarnessId"`
			Future    int    `json:"future"`
		}
		FutureTop bool `json:"futureTop"`
	}
	if err := json.Unmarshal(selected, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].Current != "new" || got.Sessions[0].HarnessID != "old" || got.Sessions[0].Future != 42 || !got.FutureTop {
		t.Fatalf("selection changed history: %s", selected)
	}
}
