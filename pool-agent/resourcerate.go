package poolagent

import "time"

// vcpuRate turns two cumulative CPU counters into vCPU-equivalents: the CPU
// microseconds burned between two samples, over the microseconds of wall clock
// that separated them. 1.0 is one core saturated; 3.7 is 3.7 cores' worth.
//
// That unit is the whole point of reporting counters rather than percentages.
// It is additive across sandboxes and comparable between them, so the column
// sorts directly into "who is eating the pool" and sums to pool load, and
// share-of-pool is just vcpus / Pool.CPUVCPUs (ADR 0071 §1).
//
// The window comes from the samples' own observation times, never from the
// tick's wall clock, so skew inside a tick cannot distort a rate.
//
// It reports false rather than a number in three cases, all of which mean "no
// rate yet" rather than "idle":
//
//   - No previous sample, which is every sandbox in the first report after the
//     agent starts.
//   - A window of zero or less, which is the same sample twice or a clock that
//     went backwards.
//   - A counter that went down, which cannot happen to a monotonic counter and
//     so means the thing being measured was replaced: a restarted container
//     with a fresh cgroup, or a recycled PID.
func vcpuRate(previousUsec, currentUsec int64, previousAt, currentAt time.Time) (float64, bool) {
	if previousAt.IsZero() || currentAt.IsZero() {
		return 0, false
	}
	window := currentAt.Sub(previousAt)
	if window <= 0 {
		return 0, false
	}
	if currentUsec < previousUsec {
		return 0, false
	}
	elapsedUsec := float64(window) / float64(time.Microsecond)
	if elapsedUsec <= 0 {
		return 0, false
	}
	return float64(currentUsec-previousUsec) / elapsedUsec, true
}

// windowSeconds is how far apart two samples were, for a consumer that wants to
// know how wide a rate's measurement window actually was. A missed report
// widens it rather than invalidating it.
func windowSeconds(previousAt, currentAt time.Time) float64 {
	if previousAt.IsZero() || currentAt.IsZero() {
		return 0
	}
	window := currentAt.Sub(previousAt)
	if window <= 0 {
		return 0
	}
	return window.Seconds()
}

// processKey identifies one process across two samples.
//
// StartTicks is in the key because PIDs are reused. Keyed on the PID alone, a
// recycled PID would be differenced against its predecessor's counter, and a
// short-lived process reusing the PID of a long-lived one would difference into
// a large negative — or, once the new process outran the old total, into a
// spike that never happened (ADR 0071 §3).
type processKey struct {
	pid        int64
	startTicks int64
}
