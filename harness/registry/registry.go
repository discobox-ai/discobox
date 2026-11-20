// Package registry is the built-in harnesses the server seeds, derived from
// their declarations (harness.Builtins) rather than from a package per
// harness (ADR 26-11-06-596 §1).
package registry

import "github.com/discobox-ai/discobox/harness"

// Definitions returns the built-in harness-config template for every
// built-in, in inventory order. The declarations are embedded and validated by
// this package's tests, so one that does not load is a build defect.
func Definitions() []harness.Definition {
	builtins, err := harness.Builtins()
	if err != nil {
		panic(err)
	}
	out := make([]harness.Definition, 0, len(builtins))
	for _, d := range builtins {
		out = append(out, d.Definition())
	}
	return out
}
