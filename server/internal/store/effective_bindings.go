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
	// Additions are, on a use binding, the sandbox's live additions of the
	// same secret in its variable, which stack on it (ADR 26-11-03-329 §2),
	// oldest first. With no plain binding in the variable, Binding is the
	// addition that lapses last and Additions are the others.
	Additions []EffectiveBinding
	// NotApplied is, for an entry of EffectiveBindings.Unapplied, why the
	// addition does not apply.
	NotApplied string
}

// EffectiveBindings are what applies at a target (ADR 26-10-27-880 §2): per
// variable, the plain use binding on the narrowest level that has one with
// the additions that stack on it (ADR 26-11-03-329 §2), and every delegate
// binding on any level. A binding that has lapsed, or whose parent has gone
// or lapsed, is not there.
type EffectiveBindings struct {
	// Use is one per variable, by variable.
	Use []EffectiveBinding
	// Delegate is narrowest first.
	Delegate []EffectiveBinding
	// Unapplied are live additions that do not apply, each with why: their
	// variable is delivered by a binding of another secret, or by one that
	// hands over the value itself, which nothing stacks on (ADR 26-11-03-329
	// Deferred). They stand, and apply again should that change within their
	// expiry.
	Unapplied []EffectiveBinding
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
	additions := map[string][]EffectiveBinding{}
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
		if binding.Adds {
			// Only a sandbox holds an addition, and only for use; the writes
			// refuse any other, so one that is not is read as nothing.
			if binding.Use && binding.TargetKind == model.SecretBindingTargetSandbox && strings.TrimSpace(binding.EnvName) != "" {
				additions[binding.EnvName] = append(additions[binding.EnvName], effective)
			}
			continue
		}
		if binding.Use && strings.TrimSpace(binding.EnvName) != "" {
			if current, ok := use[binding.EnvName]; !ok || at < current.Level {
				use[binding.EnvName] = effective
			}
		}
		if binding.Delegate && !useOnly {
			out.Delegate = append(out.Delegate, effective)
		}
	}
	for env, added := range additions {
		sortAdditions(added)
		plain, ok := use[env]
		if !ok {
			// The variable has additions alone: the one that lapses last
			// delivers it, and the others of its secret stack on it.
			last := latestLapsing(added)
			plain = added[last]
			added = append(added[:last:last], added[last+1:]...)
		}
		for _, addition := range added {
			switch {
			case addition.Binding.SecretID != plain.Binding.SecretID:
				addition.NotApplied = env + " is delivered by a binding of another secret"
				out.Unapplied = append(out.Unapplied, addition)
			case plain.Binding.Raw != "":
				addition.NotApplied = env + " is delivered by a binding that hands over the value itself, which nothing stacks on"
				out.Unapplied = append(out.Unapplied, addition)
			default:
				plain.Additions = append(plain.Additions, addition)
			}
		}
		use[env] = plain
	}
	for _, binding := range use {
		out.Use = append(out.Use, binding)
	}
	sort.Slice(out.Unapplied, func(i, j int) bool { return out.Unapplied[i].Binding.ID < out.Unapplied[j].Binding.ID })
	sort.Slice(out.Use, func(i, j int) bool { return out.Use[i].Binding.EnvName < out.Use[j].Binding.EnvName })
	sort.SliceStable(out.Delegate, func(i, j int) bool { return out.Delegate[i].Level < out.Delegate[j].Level })
	return out
}

// sortAdditions orders additions oldest first, ties by ID.
func sortAdditions(added []EffectiveBinding) {
	sort.SliceStable(added, func(i, j int) bool {
		a, b := added[i].Binding, added[j].Binding
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

// latestLapsing is the index of the addition that stops authorizing last,
// the first of those that tie: one with no expiry, which an addition is
// never given but a write could leave, lapses after every one that has.
func latestLapsing(added []EffectiveBinding) int {
	best := 0
	for i := 1; i < len(added); i++ {
		a := BindingExpiry(&added[i].Binding, added[i].Parent)
		b := BindingExpiry(&added[best].Binding, added[best].Parent)
		if b != nil && (a == nil || a.After(*b)) {
			best = i
		}
	}
	return best
}

// BindingExpiry is when a binding stops authorizing: its own expiry, or its
// parent's when that comes first. Nil is never.
func BindingExpiry(binding, parent *model.SecretBinding) *time.Time {
	expires := binding.ExpiresAt
	if parent != nil && parent.ExpiresAt != nil && (expires == nil || parent.ExpiresAt.Before(*expires)) {
		expires = parent.ExpiresAt
	}
	return expires
}
