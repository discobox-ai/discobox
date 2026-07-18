package poolruntime

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StringList accepts either a JSON array of strings or a comma-separated string.
type StringList []string

func (l *StringList) UnmarshalJSON(data []byte) error {
	var values []string
	if err := json.Unmarshal(data, &values); err == nil {
		*l = CleanStringList(values)
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*l = CleanStringList(strings.Split(value, ","))
	return nil
}

func (l StringList) Values() []string { return CleanStringList(l) }

func CleanStringList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func DecodeConfig[T any](data json.RawMessage, providerType string) (T, error) {
	var cfg T
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("decode %s provider config: %w", providerType, err)
		}
	}
	return cfg, nil
}

func RequireControlPlaneURL(providerType, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s controlPlaneUrl is required", providerType)
	}
	return nil
}
