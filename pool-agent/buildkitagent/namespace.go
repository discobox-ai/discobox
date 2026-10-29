package buildkitagent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	controlapi "github.com/moby/buildkit/api/services/control"
	gatewayapi "github.com/moby/buildkit/frontend/gateway/pb"
	spb "github.com/moby/buildkit/sourcepolicy/pb"
)

// A build is bound to its sandbox's namespace in the pool registry (ADR
// 26-10-29-160 §2). A sandbox names the registry as RegistryFrontHost, which
// only its proxy answers; buildkitd reaches it at RegistryHost. The mediator
// rewrites the one to the other in everything a solve names, and only within
// the asking sandbox's namespace, so no sandbox can have the builder write into
// another's or read from it.
//
// Matching names is enough because the registry admits buildkitd only at
// RegistryHost, the one host its certificate is presented for (registrytls.go):
// any other name for the registry's address reaches nothing, so the literal
// address is the one a solve could slip past this binding, and it is refused
// wherever a sandbox could write it.

// imageExporters are the exporter types that push to a registry by name. The
// others (docker, oci, tar, local) send their result back to the client, and a
// name there is only a label on it.
var imageExporters = map[string]bool{"image": true, "registry": true}

// bindToNamespace confines a solve to the namespace of sandbox id: every image
// it exports, and every cache it imports from or exports to the registry, is
// under that namespace and named where buildkitd reaches it, and every image it
// builds from at the registry is read from there too. A solve that names
// anything else at the registry, or exports an image anywhere but the
// registry, is refused.
func bindToNamespace(req *controlapi.SolveRequest, id string) error {
	if !validNamespace(id) {
		return fmt.Errorf("sandbox %q has no namespace in the pool registry", id)
	}
	for _, exporter := range req.Exporters {
		if exporter != nil && imageExporters[exporter.Type] {
			if err := bindImageNames(exporter.Attrs, id); err != nil {
				return err
			}
		}
	}
	if imageExporters[req.ExporterDeprecated] {
		if err := bindImageNames(req.ExporterAttrsDeprecated, id); err != nil {
			return err
		}
	}
	if err := bindFrontendCacheImports(req.FrontendAttrs, id); err != nil {
		return err
	}
	if cache := req.Cache; cache != nil {
		for _, entry := range append(append([]*controlapi.CacheOptionsEntry{}, cache.Exports...), cache.Imports...) {
			if entry == nil || entry.Type != "registry" || entry.Attrs["ref"] == "" {
				continue
			}
			ref, err := bindRef(entry.Attrs["ref"], id)
			if err != nil {
				return err
			}
			entry.Attrs["ref"] = ref
		}
		if cache.ExportRefDeprecated != "" {
			ref, err := bindRef(cache.ExportRefDeprecated, id)
			if err != nil {
				return err
			}
			cache.ExportRefDeprecated = ref
		}
		for i, imported := range cache.ImportRefsDeprecated {
			ref, err := bindRef(imported, id)
			if err != nil {
				return err
			}
			cache.ImportRefsDeprecated[i] = ref
		}
	}
	req.SourcePolicy = withNamespaceSources(req.SourcePolicy, id)
	return nil
}

// bindGatewaySolve binds a gateway solve, where buildx carries a dockerfile
// build's options and the cache it imports, to the namespace of sandbox id.
// Its sources are held by the source policy bindToNamespace put on the build.
func bindGatewaySolve(req *gatewayapi.SolveRequest, id string) error {
	if !validNamespace(id) {
		return fmt.Errorf("sandbox %q has no namespace in the pool registry", id)
	}
	if err := bindFrontendCacheImports(req.FrontendOpt, id); err != nil {
		return err
	}
	for _, entry := range req.CacheImports {
		if entry == nil || entry.Type != "registry" || entry.Attrs["ref"] == "" {
			continue
		}
		ref, err := bindRef(entry.Attrs["ref"], id)
		if err != nil {
			return err
		}
		entry.Attrs["ref"] = ref
	}
	return nil
}

// bindFrontendCacheImports binds the cache a frontend is told to import, in
// both the attributes it reads it from: cache-imports, JSON of cache entries,
// and the older cache-from, a list of registry references.
func bindFrontendCacheImports(attrs map[string]string, id string) error {
	if raw := attrs[frontendCacheImports]; raw != "" {
		var entries []*controlapi.CacheOptionsEntry
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return fmt.Errorf("read %s: %w", frontendCacheImports, err)
		}
		for _, entry := range entries {
			if entry == nil || entry.Type != "registry" || entry.Attrs["ref"] == "" {
				continue
			}
			ref, err := bindRef(entry.Attrs["ref"], id)
			if err != nil {
				return err
			}
			entry.Attrs["ref"] = ref
		}
		bound, err := json.Marshal(entries)
		if err != nil {
			return err
		}
		attrs[frontendCacheImports] = string(bound)
	}
	if raw := attrs[frontendCacheFrom]; raw != "" {
		refs := strings.Split(raw, ",")
		for i, ref := range refs {
			bound, err := bindRef(strings.TrimSpace(ref), id)
			if err != nil {
				return err
			}
			refs[i] = bound
		}
		attrs[frontendCacheFrom] = strings.Join(refs, ",")
	}
	return nil
}

// The frontend attributes a dockerfile build's cache imports arrive in.
const (
	frontendCacheImports = "cache-imports"
	frontendCacheFrom    = "cache-from"
)

// bindImageNames rewrites an image exporter's names — comma-separated, as
// BuildKit takes them — to where buildkitd pushes, refusing any not under
// the namespace.
func bindImageNames(attrs map[string]string, id string) error {
	names := attrs["name"]
	if names == "" {
		return nil
	}
	parts := strings.Split(names, ",")
	for i, name := range parts {
		name = strings.TrimSpace(name)
		rest, ok := strings.CutPrefix(name, namespacePrefix(RegistryFrontHost, id))
		if !ok || rest == "" {
			return fmt.Errorf("an image export may only name %s, not %q", namespacePrefix(RegistryFrontHost, id), name)
		}
		parts[i] = namespacePrefix(RegistryHost, id) + rest
	}
	attrs["name"] = strings.Join(parts, ",")
	return nil
}

// bindRef rewrites a reference at the registry to where buildkitd reaches it,
// refusing one outside the namespace, and one naming the registry by its
// address, which only this rewrite may write. A reference to any other
// registry is returned as it is: buildkitd reaches those itself.
func bindRef(ref, id string) (string, error) {
	if strings.HasPrefix(ref, RegistryHost+"/") {
		return "", fmt.Errorf("a build names the pool registry as %s, not %q", RegistryFrontHost, ref)
	}
	if !strings.HasPrefix(ref, RegistryFrontHost+"/") {
		return ref, nil
	}
	rest, ok := strings.CutPrefix(ref, namespacePrefix(RegistryFrontHost, id))
	if !ok || rest == "" {
		return "", fmt.Errorf("a build may only name %s at the pool registry, not %q", namespacePrefix(RegistryFrontHost, id), ref)
	}
	return namespacePrefix(RegistryHost, id) + rest, nil
}

func namespacePrefix(host, id string) string { return host + "/" + id + "/" }

// withNamespaceSources adds to a solve's source policy the rules that bind the
// images a build reads at the registry to the sandbox's namespace. A source
// policy applies to every source the build resolves — a Dockerfile's bases, a
// named context, the frontend's own image — including what the dockerfile
// frontend resolves inside the daemon, where the mediator cannot see a name.
//
//   - A convert reads an image the sandbox names under its namespace at the
//     registry from where buildkitd reaches it: a local base image a build is
//     pointed at (ADR 0047) is one. It goes first: a convert ends the pass that
//     matches it, and the rest of the rules then see the source as buildkitd
//     will fetch it.
//   - A deny refuses the registry's address, and an allow takes the
//     namespace's own back out of it. They go last, because the last allow or
//     deny that matches is the one that holds: no rule of the sandbox's own,
//     before them, can allow another namespace.
func withNamespaceSources(policy *spb.Policy, id string) *spb.Policy {
	if policy == nil {
		policy = &spb.Policy{Version: 1}
	}
	convert := &spb.Rule{
		Action: spb.PolicyAction_CONVERT,
		Selector: &spb.Selector{
			Identifier: "^docker-image://" + regexp.QuoteMeta(namespacePrefix(RegistryFrontHost, id)) + "(.+)$",
			MatchType:  spb.MatchType_REGEX,
		},
		Updates: &spb.Update{Identifier: "docker-image://" + namespacePrefix(RegistryHost, id) + "${1}"},
	}
	address := "^docker-image://" + regexp.QuoteMeta(RegistryHost+"/")
	deny := &spb.Rule{
		Action:   spb.PolicyAction_DENY,
		Selector: &spb.Selector{Identifier: address, MatchType: spb.MatchType_REGEX},
	}
	allow := &spb.Rule{
		Action:   spb.PolicyAction_ALLOW,
		Selector: &spb.Selector{Identifier: "^docker-image://" + regexp.QuoteMeta(namespacePrefix(RegistryHost, id)), MatchType: spb.MatchType_REGEX},
	}
	policy.Rules = append(append([]*spb.Rule{convert}, policy.Rules...), deny, allow)
	return policy
}
