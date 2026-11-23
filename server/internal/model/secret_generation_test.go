package model

import (
	"testing"
	"time"
)

func TestValueWrittenAdvancesGeneration(t *testing.T) {
	previous := time.Date(2026, 11, 11, 12, 0, 0, 123456000, time.UTC)
	for _, delta := range []time.Duration{0, 100 * time.Nanosecond, -time.Second, time.Second} {
		t.Run(delta.String(), func(t *testing.T) {
			for _, legacy := range []bool{false, true} {
				secret := Secret{CreatedAt: previous}
				if !legacy {
					secret.ValueUpdatedAt = &previous
				}
				generation := secret.ValueGeneration()
				expires := previous.Add(time.Hour)
				secret.ValueWritten(previous.Add(delta), &expires)
				if secret.ValueGeneration() == generation || secret.ValueWrittenAt().UnixMicro() <= previous.UnixMicro() {
					t.Fatalf("legacy=%v: write at %v left generation %q at %v", legacy, previous.Add(delta), secret.ValueGeneration(), secret.ValueWrittenAt())
				}
				if secret.ValueExpiresAt == nil || !secret.ValueExpiresAt.Equal(expires) {
					t.Fatal("value expiry changed")
				}
				if delta >= time.Microsecond && !secret.ValueWrittenAt().Equal(previous.Add(delta)) {
					t.Fatal("later write time changed")
				}
			}
		})
	}
}

// Two secrets whose values were written in the same microsecond are two
// generations: a variable rebound from one to the other must make the proxy
// drop the first one's value (ADR 26-11-19-927 §5).
func TestValueGenerationNamesItsSecret(t *testing.T) {
	written := time.Date(2026, 11, 19, 12, 0, 0, 123456000, time.UTC)
	a := Secret{ID: "sec_a", ValueUpdatedAt: &written}
	b := Secret{ID: "sec_b", ValueUpdatedAt: &written}
	if a.ValueGeneration() == b.ValueGeneration() {
		t.Fatalf("secrets %s and %s written at the same instant share generation %q", a.ID, b.ID, a.ValueGeneration())
	}
}
