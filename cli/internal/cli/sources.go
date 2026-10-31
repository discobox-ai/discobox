package cli

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

type sourceEntry struct {
	slug   string
	source apimodel.GitSource
}

func parseDirOverrides(values []string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, value := range values {
		slug, dir, ok := strings.Cut(value, "=")
		slug, dir = strings.TrimSpace(slug), strings.TrimSpace(dir)
		if !ok || slug == "" || dir == "" {
			return nil, fmt.Errorf("--dir must be in slug=path form, got %q", value)
		}
		out[slug] = dir
	}
	return out, nil
}

func sandboxSources(sandbox *apimodel.Sandbox) []sourceEntry {
	var out []sourceEntry
	if source, ok := sandbox.Config.Source.Get(); ok {
		if slug, ok := source.Slug.Get(); ok && slug != "" {
			out = append(out, sourceEntry{slug: slug, source: source})
		}
	}
	if refs, ok := sandbox.Config.SourceCodeReferences.Get(); ok {
		keys := make([]string, 0, len(refs))
		for key := range refs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			source := refs[key]
			if slug, ok := source.Slug.Get(); ok && slug != "" {
				out = append(out, sourceEntry{slug: slug, source: source})
			}
		}
	}
	return out
}

func selectSources(sandbox *apimodel.Sandbox, onlySlug string) ([]sourceEntry, error) {
	sources := sandboxSources(sandbox)
	if onlySlug == "" {
		worked := sources[:0]
		for _, s := range sources {
			if s.source.Purpose.Or("") != apiclientgen.GitSourcePurposeSkills {
				worked = append(worked, s)
			}
		}
		if len(worked) == 0 && len(sources) > 0 {
			return nil, fmt.Errorf("discobox's only sources are skills sources; name one with --source")
		}
		sources = worked
	}
	if onlySlug != "" {
		filtered := sources[:0]
		for _, s := range sources {
			if s.slug == onlySlug {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			return nil, fmt.Errorf("discobox has no source with slug %q", onlySlug)
		}
		sources = filtered
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("discobox has no sources")
	}
	return sources, nil
}

func sourceWorkdir(source apimodel.GitSource) string {
	dest, ok := source.Destination.Get()
	if !ok {
		return ""
	}
	if wd, ok := dest.WorkingDirectory.Get(); ok {
		return wd
	}
	if dir, ok := dest.Directory.Get(); ok {
		return dir
	}
	return ""
}

func resolveSourceHostDir(sandbox *apimodel.Sandbox, hostID string, entry sourceEntry, dirOverrides map[string]string) (string, error) {
	if dir, ok := dirOverrides[entry.slug]; ok {
		return dir, nil
	}
	needDir := fmt.Sprintf("pass --dir %s=PATH", entry.slug)

	origin, hasOrigin := sandbox.Origin.Get()
	if !hasOrigin {
		return "", fmt.Errorf("the discobox has no recorded origin, so nothing says which directory it came from; %s", needDir)
	}
	if origin.HostId != hostID {
		return "", fmt.Errorf("the discobox was created on a different machine (origin host %s%s, this machine is %s); %s",
			origin.HostId, formatOriginHostname(origin), hostID, needDir)
	}

	local := strings.TrimSpace(entry.source.LocalDirectory.Or(""))
	if local == "" {
		return "", fmt.Errorf("source %q has no local directory recorded — it was cloned from a remote rather than pushed from a checkout here; %s", entry.slug, needDir)
	}
	info, err := os.Stat(local)
	if err != nil {
		return "", fmt.Errorf("the directory source %q came from, %s, is not readable on this machine (%w); %s", entry.slug, local, err, needDir)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("the path source %q came from, %s, is no longer a directory; %s", entry.slug, local, needDir)
	}
	return local, nil
}

func formatOriginHostname(origin apimodel.Origin) string {
	if hostname := strings.TrimSpace(origin.Hostname.Or("")); hostname != "" {
		return " " + strconv.Quote(hostname)
	}
	return ""
}
