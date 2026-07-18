package dockerworker

import (
	"encoding/json"
	"sort"
	"strings"
)

// HostMount describes a host path mounted into pool-agent containers under
// the host-mount target root.
type HostMount struct {
	Source   string `json:"source,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

func (m HostMount) MarshalJSON() ([]byte, error) {
	mode := "rw"
	if m.ReadOnly {
		mode = "ro"
	}
	return json.Marshal(cleanAbsPath(m.Source) + ":" + mode)
}

func (m *HostMount) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*m = parseHostMount(value)
		return nil
	}
	var object struct {
		Source   string `json:"source"`
		ReadOnly bool   `json:"readOnly"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	*m = HostMount{Source: object.Source, ReadOnly: object.ReadOnly}
	return nil
}

func parseHostMount(value string) HostMount {
	value = strings.TrimSpace(value)
	readOnly := false
	for _, suffix := range []string{":ro", ":rw"} {
		if strings.HasSuffix(value, suffix) {
			readOnly = suffix == ":ro"
			value = strings.TrimSuffix(value, suffix)
			break
		}
	}
	return HostMount{Source: value, ReadOnly: readOnly}
}

// NormalizeHostMounts cleans, deduplicates, and sorts host mounts.
func NormalizeHostMounts(hostMounts []HostMount) []HostMount {
	out := make([]HostMount, 0, len(hostMounts))
	seen := map[string]struct{}{}
	for _, hostMount := range hostMounts {
		source := cleanAbsPath(hostMount.Source)
		if source == "" {
			continue
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, HostMount{Source: source, ReadOnly: hostMount.ReadOnly})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}

func hasHostMountSource(hostMounts []HostMount, source string) bool {
	source = cleanAbsPath(source)
	for _, hostMount := range hostMounts {
		if hostMount.Source == source {
			return true
		}
	}
	return false
}

func cleanAbsPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || !strings.HasPrefix(path, "/") {
		return ""
	}
	parts := make([]string, 0, strings.Count(path, "/")+1)
	for _, part := range strings.Split(path, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "/"
	}
	return "/" + strings.Join(parts, "/")
}

func hostMountTarget(source string) string {
	source = cleanAbsPath(source)
	source = strings.TrimPrefix(source, "/")
	if source == "" {
		return hostMountTargetRoot
	}
	return hostMountTargetRoot + "/" + source
}
