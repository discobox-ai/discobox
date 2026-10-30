package poolruntime

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/transport/carrierhub"
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

func DecodeConfig[T any](data []byte, providerType string) (T, error) {
	var cfg T
	if len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("decode %s provider config: %w", providerType, err)
		}
	}
	return cfg, nil
}

// ControlPlaneStreams is how a remote provider's pools reach their control
// plane (ADR 26-10-30-875 §4). Given a controlPlaneUrl, they dial it, and it is
// nil. Without one, the server opens a channel to each pool, and it is hub:
// the carrier hub every stream the pools open on one is pushed to.
func ControlPlaneStreams(providerType, controlPlaneURL string, hub *carrierhub.Hub) (StreamSink, error) {
	if strings.TrimSpace(controlPlaneURL) != "" {
		return nil, nil
	}
	if hub == nil {
		return nil, fmt.Errorf("%s: this server cannot open control-plane channels to its pools, so it needs a controlPlaneUrl", providerType)
	}
	return hub, nil
}
