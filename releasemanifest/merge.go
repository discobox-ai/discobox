package releasemanifest

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/discobox-ai/discobox/serverstage"
)

// Merge assembles platform build results into one complete release inventory.
// A platform must supply both binaries, and all builds must agree on source
// revision and image roles; a partially mixed release is never published.
func Merge(parts []Manifest) (Manifest, error) {
	if len(parts) == 0 {
		return Manifest{}, fmt.Errorf("no platform manifests supplied")
	}
	out := parts[0]
	if out.Revision == "" {
		return Manifest{}, fmt.Errorf("release source revision is required")
	}
	out.Servers = nil
	out.Clients = nil
	for _, part := range parts {
		if err := part.Validate(); err != nil {
			return Manifest{}, err
		}
		if part.Version != out.Version || part.Revision != out.Revision || !reflect.DeepEqual(part.Images, out.Images) {
			return Manifest{}, fmt.Errorf("platform manifests disagree on release version, revision, or images")
		}
		out.Servers = append(out.Servers, part.Servers...)
		out.Clients = append(out.Clients, part.Clients...)
	}
	if err := out.Validate(); err != nil {
		return Manifest{}, err
	}
	if len(out.Servers) == 0 || len(out.Servers) != len(out.Clients) {
		return Manifest{}, fmt.Errorf("release must include a CLI and server for every platform")
	}
	platforms := map[string]bool{}
	for _, server := range out.Servers {
		platforms[server.Platform()] = true
	}
	for _, client := range out.Clients {
		if !platforms[client.Platform()] {
			return Manifest{}, fmt.Errorf("CLI platform %s has no matching server", client.Platform())
		}
	}
	for _, binaries := range [][]serverstage.Manifest{out.Servers, out.Clients} {
		sort.Slice(binaries, func(i, j int) bool { return binaries[i].Platform() < binaries[j].Platform() })
	}
	return out, nil
}
