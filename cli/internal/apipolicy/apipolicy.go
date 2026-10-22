// Package apipolicy converts a credential policy between the shape the API
// carries (apimodel.AccessPolicy) and the policy package's, which is what is
// parsed, rendered and compared. The generated type encodes and decodes
// itself, so the conversion goes through its JSON, which is the one shape both
// agree on.
package apipolicy

import (
	"encoding/json"
	"fmt"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/policy"
)

// From is p as the policy package reads it.
func From(p apimodel.AccessPolicy) (policy.Policy, error) {
	// encoding/json cannot write an optional field the generated type left
	// unset, so the type encodes itself.
	data, err := p.MarshalJSON()
	if err != nil {
		return policy.Policy{}, fmt.Errorf("encode policy: %w", err)
	}
	var out policy.Policy
	if err := json.Unmarshal(data, &out); err != nil {
		return policy.Policy{}, fmt.Errorf("decode policy: %w", err)
	}
	return out, nil
}

// To is p as the API carries it, for a request that sends one.
func To(p policy.Policy) (apimodel.AccessPolicy, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return apimodel.AccessPolicy{}, fmt.Errorf("encode policy: %w", err)
	}
	var out apimodel.AccessPolicy
	if err := out.UnmarshalJSON(data); err != nil {
		return apimodel.AccessPolicy{}, fmt.Errorf("decode policy: %w", err)
	}
	return out, nil
}
