package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// EffectiveBinding is one binding that applies at a target, with the
// delegate binding it was handed on under, when it was.
type EffectiveBinding struct {
	Binding model.SecretBinding
	Parent  *model.SecretBinding
	// Level is the index, in the targets resolved, of the one it is on:
	// zero is the narrowest.
	Level int
}

// EffectiveBindings are what applies at a target (ADR 26-10-27-880 §2): per
// variable, the use binding on the narrowest level that has one, and every
// delegate binding on any level. A binding that has lapsed, or whose parent
// has gone or lapsed, is not there.
type EffectiveBindings struct {
	// Use is one per variable, by variable.
	Use []EffectiveBinding
	// Delegate is narrowest first.
	Delegate []EffectiveBinding
}

// UseFor is the use binding delivering envName, if one applies.
func (e EffectiveBindings) UseFor(envName string) (EffectiveBinding, bool) {
	for _, binding := range e.Use {
		if binding.Binding.EnvName == envName {
			return binding, true
		}
	}
	return EffectiveBinding{}, false
}

// BindingTargetsAt are the levels that cover a target, narrowest first: a
// sandbox's are SandboxBindingTargets; a harness config is covered by itself
// and its project; a project by itself.
func BindingTargetsAt(projectID, kind, targetID string) []BindingTarget {
	switch kind {
	case model.SecretBindingTargetHarnessConfig:
		return []BindingTarget{{Kind: kind, ID: targetID}, {Kind: model.SecretBindingTargetProject, ID: projectID}}
	case model.SecretBindingTargetProject:
		return []BindingTarget{{Kind: kind, ID: projectID}}
	}
	return nil
}

// ResolveBindings reads what applies at targets, narrowest first, at now.
func (s *Store) ResolveBindings(ctx context.Context, projectID string, targets []BindingTarget, now time.Time) (EffectiveBindings, error) {
	bindings, err := s.ListBindingsOnTargets(ctx, projectID, targets)
	if err != nil {
		return EffectiveBindings{}, err
	}
	var parentIDs []string
	for _, binding := range bindings {
		if binding.ParentID != "" {
			parentIDs = append(parentIDs, binding.ParentID)
		}
	}
	parents, err := s.ListBindingsByIDs(ctx, projectID, parentIDs)
	if err != nil {
		return EffectiveBindings{}, err
	}
	return resolveBindings(bindings, parents, targets, now), nil
}

// ResolveSandboxBindings reads what applies to a sandbox at now.
func (s *Store) ResolveSandboxBindings(ctx context.Context, sandbox *model.Sandbox, now time.Time) (EffectiveBindings, error) {
	return s.ResolveBindings(ctx, sandbox.ProjectID, SandboxBindingTargets(sandbox), now)
}

// resolveBindings picks what applies from the bindings on targets.
func resolveBindings(bindings []model.SecretBinding, parents map[string]model.SecretBinding, targets []BindingTarget, now time.Time) EffectiveBindings {
	level := func(binding model.SecretBinding) (int, bool) {
		for i, target := range targets {
			if target.Kind == binding.TargetKind && target.ID == binding.TargetID {
				return i, target.UseOnly
			}
		}
		return -1, false
	}
	var out EffectiveBindings
	use := map[string]EffectiveBinding{}
	for _, binding := range bindings {
		at, useOnly := level(binding)
		if at < 0 || !binding.Live(now) {
			continue
		}
		var parent *model.SecretBinding
		if binding.ParentID != "" {
			p, ok := parents[binding.ParentID]
			if !ok || !p.Live(now) {
				continue
			}
			parent = &p
		}
		effective := EffectiveBinding{Binding: binding, Parent: parent, Level: at}
		if binding.Use && strings.TrimSpace(binding.EnvName) != "" {
			if current, ok := use[binding.EnvName]; !ok || at < current.Level {
				use[binding.EnvName] = effective
			}
		}
		if binding.Delegate && !useOnly {
			out.Delegate = append(out.Delegate, effective)
		}
	}
	for _, binding := range use {
		out.Use = append(out.Use, binding)
	}
	sort.Slice(out.Use, func(i, j int) bool { return out.Use[i].Binding.EnvName < out.Use[j].Binding.EnvName })
	sort.SliceStable(out.Delegate, func(i, j int) bool { return out.Delegate[i].Level < out.Delegate[j].Level })
	return out
}
