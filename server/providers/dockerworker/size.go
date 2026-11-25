package dockerworker

import sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"

// SizeFields are the size fields every provider built on this engine acts on,
// for its ProviderDefinition: each of its boxes is a container of its pool
// agent's Docker runtime, limited to the box's CPU and memory, and a pool's
// CPU and memory are its boxes' default (ADR 26-11-23-445 §§3, 4). A provider
// that also sizes its pool's VM from them reads the same two fields (VMSpec),
// so the one declaration covers both.
func SizeFields() []sandbox.SizeField {
	return []sandbox.SizeField{sandbox.SizeCPU, sandbox.SizeMemory}
}
