package judge

import "testing"

func TestDecodeRequiresAnUnambiguousCompleteVerdict(t *testing.T) {
	for _, input := range []string{`{}`, `{"allow":true}`, `{"allow":null,"reason":"ok"}`, `{"allow":"true","reason":"ok"}`, `{"allow":true,"reason":""}`, `{"allow":false,"allow":true,"reason":"ok"}`, `{"allow":true,"reason":"ok","tools":[]}`, `{"allow":true,"reason":"ok"} garbage`, `transcript {"allow":true,"reason":"ok"}`} {
		if _, _, err := Decode([]byte(input)); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
	allow, reason, err := Decode([]byte(`{"allow":true,"reason":"supports the approved PR"}`))
	if err != nil || !allow || reason == "" {
		t.Fatalf("valid verdict: %v %s %v", allow, reason, err)
	}
}
