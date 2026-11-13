package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// A ProviderConfigField's Key is a path into the provider's config object:
// its segments, separated by ".", name an object member at each level. A
// field of a provider's top level is one segment ("controlPlaneUrl"); a
// field of a disco-vm driver's own configuration is two, under the object
// the driver is given ("driver.addr", ADR 26-11-11-080 §2). Every read,
// write and seal of a field goes through these, so a nested field is sealed,
// kept, compared and cleared as a top-level one is.

// ConfigValue is the value at key's path in config, and whether it is set.
// A path through a member that is not an object reaches nothing.
func ConfigValue(config map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	segments := strings.Split(key, ".")
	current := config
	for i, segment := range segments {
		value, ok := current[segment]
		if !ok {
			return nil, false
		}
		if i == len(segments)-1 {
			return value, true
		}
		current = nil
		if json.Unmarshal(value, &current) != nil || current == nil {
			return nil, false
		}
	}
	return nil, false
}

// SetConfigValue sets value at key's path in config, making each object on
// the way that is missing. A member on the way that is set and not an object
// is an error: a path never replaces one.
func SetConfigValue(config map[string]json.RawMessage, key string, value json.RawMessage) error {
	segments := strings.Split(key, ".")
	if len(segments) == 1 {
		config[key] = value
		return nil
	}
	parent := map[string]json.RawMessage{}
	if raw, ok := config[segments[0]]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &parent); err != nil || parent == nil {
			return fmt.Errorf("provider config %s is not an object", segments[0])
		}
	}
	if err := SetConfigValue(parent, strings.Join(segments[1:], "."), value); err != nil {
		return fmt.Errorf("%s.%w", segments[0], err)
	}
	encoded, err := json.Marshal(parent)
	if err != nil {
		return err
	}
	config[segments[0]] = encoded
	return nil
}

// DeleteConfigValue takes key's path out of config, and returns what was
// there. An object it empties stays, so a driver's configuration given as {}
// is still given.
func DeleteConfigValue(config map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	segments := strings.Split(key, ".")
	if len(segments) == 1 {
		value, ok := config[key]
		delete(config, key)
		return value, ok
	}
	raw, ok := config[segments[0]]
	if !ok {
		return nil, false
	}
	var parent map[string]json.RawMessage
	if json.Unmarshal(raw, &parent) != nil || parent == nil {
		return nil, false
	}
	value, ok := DeleteConfigValue(parent, strings.Join(segments[1:], "."))
	if !ok {
		return nil, false
	}
	encoded, err := json.Marshal(parent)
	if err != nil {
		return nil, false
	}
	config[segments[0]] = encoded
	return value, true
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
