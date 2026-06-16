package api

import (
	"encoding/json"
)

func Convert[To any](from any) (To, error) {
	var to To
	data, err := json.Marshal(from)
	if err != nil {
		return to, err
	}
	if err := json.Unmarshal(data, &to); err != nil {
		return to, err
	}
	return to, nil
}

func OptStringPtr(value OptString) *string {
	if v, ok := value.Get(); ok {
		return &v
	}
	return nil
}

func OptURIStringPtr(value OptURI) *string {
	if v, ok := value.Get(); ok {
		s := v.String()
		return &s
	}
	return nil
}

func OptIntPtr(value OptInt64) *int {
	if v, ok := value.Get(); ok {
		i := int(v)
		return &i
	}
	return nil
}

func OptStringValue(value OptString) (string, bool) {
	return value.Get()
}

func RawMessage(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return json.RawMessage(raw)
}
