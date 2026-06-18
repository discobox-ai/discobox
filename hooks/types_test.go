package hooks

import "testing"

func TestHookTypeValid(t *testing.T) {
	for _, typ := range []HookType{HookTypeSession, HookTypeFile, HookTypePreCommit} {
		if !typ.Valid() {
			t.Fatalf("expected %q valid", typ)
		}
	}
	if HookType("other").Valid() {
		t.Fatal("expected unknown hook type invalid")
	}
}

func TestHookEngineValid(t *testing.T) {
	for _, engine := range []HookEngine{HookEngineScript, HookEngineAI, HookEngineBuiltin} {
		if !engine.Valid() {
			t.Fatalf("expected %q valid", engine)
		}
	}
	if HookEngine("native").Valid() {
		t.Fatal("expected unknown hook engine invalid")
	}
}
