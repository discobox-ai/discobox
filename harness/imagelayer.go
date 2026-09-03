package harness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	// ImageLayerLabelPrefix prefixes a *contributed* manifest layer's label
	// key. The suffix is the layer's name, conventionally "<NN>-<what>", and
	// layers merge in ascending lexical order of that suffix before the image's
	// own ImageLabel layer, which is always last (ADR 0086 §2).
	//
	// Ordering lives in the key rather than the payload so a layer's position
	// is fixed by the author who set it and readable without parsing anything,
	// the way a *.d directory orders its fragments.
	ImageLayerLabelPrefix = ImageLabel + "."

	// SandboxBaseLayer is the layer the sandbox agent base image contributes:
	// the env, volumes, groups, and seed files that are true of every image
	// built FROM it. Its presence is also what proves an image *was* built from
	// that base, which registration requires (ADR 0086 §1).
	SandboxBaseLayer = "10-sandbox-base"

	// MetadataBuildArg and LayerMetadataBuildArg are the build arguments a
	// Dockerfile turns into its own ImageLabel and its contributed layer label
	// respectively. They are part of the build contract rather than either
	// consumer's private detail: the image watcher fills them in, and the
	// server reads them straight back out of a development manifest for an
	// image no daemon has built yet. Keep each in sync with the Dockerfile that
	// declares the matching ARG.
	MetadataBuildArg      = "HARNESS_METADATA"
	LayerMetadataBuildArg = "IMAGE_LAYER_METADATA"

	// LayerNumberReserved is the exclusive upper bound of the "<NN>-" range
	// Discobox reserves for its own layers. A downstream image numbers from
	// here so it cannot silently replace one of ours by reusing its key.
	LayerNumberReserved = 50
)

// ImageLayer is one manifest layer with the label-key suffix it was named by.
type ImageLayer struct {
	Name     string
	Metadata ImageMetadata
}

// ResolveImageLabels returns the effective manifest an image's label set
// declares: every contributed layer, in ascending order of its name, with the
// image's own layer merged last.
//
// It reports whether a base layer was found. An image carrying none was not
// built FROM the sandbox agent base, which the caller rejects — the label set
// is the only evidence of lineage available without pulling filesystem layers.
//
// Layers are fragments, not manifests: none of them is validated here, because
// a base layer legitimately has no harness, no id, and no name. Only the merged
// result is an image contract, and only the caller validates it.
func ResolveImageLabels(labels map[string]string) (metadata ImageMetadata, hasBase bool, err error) {
	layers := make([]ImageLayer, 0, len(labels))
	for key, raw := range labels {
		name, ok := layerName(key)
		if !ok {
			continue
		}
		layer, err := parseImageLayer(name, raw)
		if err != nil {
			return ImageMetadata{}, false, err
		}
		if name == SandboxBaseLayer {
			hasBase = true
		}
		layers = append(layers, layer)
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].Name < layers[j].Name })

	ordered := make([]ImageMetadata, 0, len(layers)+1)
	for _, layer := range layers {
		ordered = append(ordered, layer.Metadata)
	}
	// The image's own layer is last by definition rather than by sorting: it is
	// the leaf's final word over everything it inherited. An image that
	// declares nothing has no such label at all, and is its inherited layers.
	if raw, ok := labels[ImageLabel]; ok {
		own, err := parseImageLayer(ImageLabel, raw)
		if err != nil {
			return ImageMetadata{}, false, err
		}
		ordered = append(ordered, own.Metadata)
	}
	return MergeImageMetadata(ordered...), hasBase, nil
}

// layerName returns the layer name a contributed layer's label key carries.
func layerName(key string) (string, bool) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, ImageLayerLabelPrefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(key, ImageLayerLabelPrefix))
	if name == "" {
		return "", false
	}
	return name, true
}

func parseImageLayer(name, raw string) (ImageLayer, error) {
	// A present-but-empty label is a build that forgot the argument, not an
	// image that declares nothing: `LABEL key=${ARG}` with no ARG passed sets
	// the key to "". Saying which argument was missed beats "unexpected end of
	// JSON input", and beats treating the layer as absent — that would report
	// the base image as not being the base image.
	if strings.TrimSpace(raw) == "" {
		arg := MetadataBuildArg
		if name != ImageLabel {
			arg = LayerMetadataBuildArg
		}
		return ImageLayer{}, fmt.Errorf("image label %q is empty: the image was built without its %s build argument", name, arg)
	}
	var metadata ImageMetadata
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &metadata); err != nil {
		return ImageLayer{}, fmt.Errorf("parse image layer %q: %w", name, err)
	}
	if version := strings.TrimSpace(metadata.APIVersion); version != "" && version != ImageAPIVersion {
		return ImageLayer{}, fmt.Errorf("image layer %q declares unsupported apiVersion %q; want %q", name, version, ImageAPIVersion)
	}
	return ImageLayer{Name: name, Metadata: metadata}, nil
}

// MergeImageMetadata merges layers in the order given, later over earlier.
//
// Lists merge by identity, never by position: a volume by its path, a file by
// its path, a secret by its name. A later layer replaces the entry in place and
// appends the ones it adds, so a base's ordering survives a leaf overriding one
// member of it. Positional merging would make "the third volume" load-bearing
// between files written by people who have never seen each other's.
//
// There is no unset: a later layer overrides an entry and removes none.
func MergeImageMetadata(layers ...ImageMetadata) ImageMetadata {
	var out ImageMetadata
	for _, layer := range layers {
		if version := strings.TrimSpace(layer.APIVersion); version != "" {
			out.APIVersion = version
		}
		out.Env = mergeEnv(out.Env, layer.Env)
		out.Volumes = mergeVolumes(out.Volumes, layer.Volumes)
		out.AdditionalGroups = mergeGroups(out.AdditionalGroups, layer.AdditionalGroups)
		out.Harness = mergeHarness(out.Harness, layer.Harness)
	}
	return out
}

func mergeEnv(base, over map[string]string) map[string]string {
	if len(over) == 0 {
		return base
	}
	if base == nil {
		base = make(map[string]string, len(over))
	}
	for key, value := range over {
		if key = strings.TrimSpace(key); key != "" {
			base[key] = value
		}
	}
	return base
}

func mergeVolumes(base, over []Volume) []Volume {
	for _, volume := range over {
		key := strings.TrimSpace(volume.Path)
		replaced := false
		for i := range base {
			if strings.TrimSpace(base[i].Path) == key {
				base[i], replaced = volume, true
				break
			}
		}
		if !replaced {
			base = append(base, volume)
		}
	}
	return base
}

func mergeGroups(base, over []string) []string {
	for _, group := range over {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		seen := false
		for _, existing := range base {
			if existing == group {
				seen = true
				break
			}
		}
		if !seen {
			base = append(base, group)
		}
	}
	return base
}

func mergeHarness(base, over *Image) *Image {
	if over == nil {
		return base
	}
	if base == nil {
		base = &Image{}
	}
	merged := *base
	if id := strings.TrimSpace(over.ID); id != "" {
		merged.ID = id
	}
	if name := strings.TrimSpace(over.Name); name != "" {
		merged.Name = name
	}
	if description := strings.TrimSpace(over.Description); description != "" {
		merged.Description = description
	}
	if len(over.RunCommand) > 0 {
		merged.RunCommand = append([]string{}, over.RunCommand...)
	}
	if len(over.RelaunchCommand) > 0 {
		merged.RelaunchCommand = append([]string{}, over.RelaunchCommand...)
	}
	if over.Config != nil {
		config := *over.Config
		config.Command = append([]string(nil), over.Config.Command...)
		config.Ports = append([]ConfigPort(nil), over.Config.Ports...)
		merged.Config = &config
	}
	merged.Files = mergeFiles(merged.Files, over.Files)
	merged.Secrets = mergeSecrets(merged.Secrets, over.Secrets)
	return &merged
}

func mergeFiles(base, over []File) []File {
	for _, file := range over {
		key := strings.TrimSpace(file.Path)
		replaced := false
		for i := range base {
			if strings.TrimSpace(base[i].Path) == key {
				base[i], replaced = file, true
				break
			}
		}
		if !replaced {
			base = append(base, file)
		}
	}
	return base
}

func mergeSecrets(base, over []Secret) []Secret {
	for _, secret := range over {
		key := strings.TrimSpace(secret.Name)
		replaced := false
		for i := range base {
			if strings.TrimSpace(base[i].Name) == key {
				base[i], replaced = secret, true
				break
			}
		}
		if !replaced {
			base = append(base, secret)
		}
	}
	return base
}
