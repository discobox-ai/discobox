// Package id generates and classifies Discobox resource IDs.
//
// A generated ID is "<prefix>_<random>": a short resource-type prefix, an
// underscore, and 16 random lowercase Crockford base32 characters (80 bits of
// entropy). The prefix makes IDs recognizable on sight and keeps tab
// completion useful; creation time lives in CreatedAt columns, not in the ID.
package id

import (
	"crypto/rand"
	"strings"
)

// Well-known resource ID prefixes. Every resource type mints IDs with a
// distinct prefix so an ID's type is recognizable on sight.
const (
	PrefixUser                       = "user"
	PrefixProject                    = "proj"
	PrefixHarnessConfig              = "harness"
	PrefixHarnessConfigSecretBinding = "bind"
	PrefixSandbox                    = "sbx"
	PrefixSandboxProvider            = "prov"
	PrefixPool                       = "pool"
	PrefixSandboxSecret              = "sbsec"
	PrefixPoolBootstrapToken         = "pbt"
	PrefixEvent                      = "evt"
	PrefixSecret                     = "sec"
	PrefixSecretRequest              = "sreq"
	PrefixSecretGrant                = "grant"
	PrefixExec                       = "ex"
	PrefixRun                        = "run"
	PrefixSnapshot                   = "snap"
	PrefixHost                       = "host"
)

// RandomLength is the length of the random portion of a generated ID.
const RandomLength = 16

// alphabet is lowercase Crockford base32: exactly 32 characters, excluding
// i, l, o, and u to avoid lookalike ambiguity.
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// New returns a new "<prefix>_<random>" ID.
func New(prefix string) (string, error) {
	var buf [RandomLength]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	for i, b := range buf {
		buf[i] = alphabet[int(b)&31]
	}
	return prefix + "_" + string(buf[:]), nil
}

// NewString is New for callers without an error path. ID generation only fails
// when the platform random source is broken, which is not recoverable.
func NewString(prefix string) string {
	id, err := New(prefix)
	if err != nil {
		panic(err)
	}
	return id
}

// IsGenerated reports whether value has the exact shape of a generated ID:
// a nonempty prefix, an underscore, and RandomLength alphabet characters.
// Well-known IDs like "user_default" and partial IDs do not match.
func IsGenerated(value string) bool {
	value = strings.TrimSpace(value)
	sep := strings.LastIndexByte(value, '_')
	if sep <= 0 || len(value)-sep-1 != RandomLength {
		return false
	}
	for i := sep + 1; i < len(value); i++ {
		if strings.IndexByte(alphabet, value[i]) < 0 {
			return false
		}
	}
	return true
}

// RandomPart returns the random portion of a generated ID, or the value
// unchanged when it has no prefix separator.
func RandomPart(id string) string {
	if sep := strings.IndexByte(id, '_'); sep >= 0 {
		return id[sep+1:]
	}
	return id
}

// ResolveShort returns the candidates a short ID selects. An exact match wins
// outright. Otherwise a short ID matches a prefix of the full ID ("sbx_dfzx")
// or, when nothing matches that way, a prefix of the random part alone
// ("dfzx"), so the distinctive tail is usable without retyping the resource
// prefix. Callers decide what zero or multiple matches mean.
func ResolveShort(short string, candidates []string) []string {
	short = strings.TrimSpace(short)
	if short == "" {
		return nil
	}
	var full, random []string
	for _, candidate := range candidates {
		if candidate == short {
			return []string{candidate}
		}
		if strings.HasPrefix(candidate, short) {
			full = append(full, candidate)
			continue
		}
		if IsGenerated(candidate) && strings.HasPrefix(RandomPart(candidate), short) {
			random = append(random, candidate)
		}
	}
	if len(full) > 0 {
		return full
	}
	return random
}
