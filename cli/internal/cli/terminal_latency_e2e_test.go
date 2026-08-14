package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obot-platform/discobox/execstream/frame"
	"github.com/obot-platform/discobox/execstream/resume"
)

const (
	terminalLatencyE2EEnv = "DISCOBOX_TERMINAL_LATENCY_E2E"
	latencyReadyMarker    = "DISCOBOX_LATENCY_READY"
)

// TestTerminalLatencyE2E is an opt-in performance probe, not a timing gate. It
// attaches through the real control plane, pool agent, sandbox agent, shim, and
// PTY to the deterministic test/performance/terminal-latency harness.
//
// The orchestration script creates and owns the disposable sandbox. This test
// owns only the attach, records exact resumable-action annotations from the
// shared transport, and writes a JSON artifact when
// DISCOBOX_TERMINAL_LATENCY_REPORT is set.
func TestTerminalLatencyE2E(t *testing.T) {
	if os.Getenv(terminalLatencyE2EEnv) != "1" {
		t.Skip("set " + terminalLatencyE2EEnv + "=1 and use go tool task perf:terminal")
	}

	serverURL := envOrDefault("DISCOBOX_TERMINAL_LATENCY_SERVER", "http://127.0.0.1:18080")
	projectID := envOrDefault("DISCOBOX_TERMINAL_LATENCY_PROJECT", defaultProjectAlias)
	sandboxID := strings.TrimSpace(os.Getenv("DISCOBOX_TERMINAL_LATENCY_SANDBOX"))
	if sandboxID == "" {
		t.Fatal("DISCOBOX_TERMINAL_LATENCY_SANDBOX is required")
	}
	samples := latencyEnvInt(t, "DISCOBOX_TERMINAL_LATENCY_SAMPLES", 100)
	sequenceStart := latencyEnvInt(t, "DISCOBOX_TERMINAL_LATENCY_SEQUENCE_START", 1)
	if sequenceStart+samples-1 > 99_999_999 {
		t.Fatal("DISCOBOX_TERMINAL_LATENCY_SEQUENCE_START + samples exceeds the eight-digit probe protocol")
	}
	interval := latencyEnvDuration(t, "DISCOBOX_TERMINAL_LATENCY_INTERVAL", 20*time.Millisecond)
	sampleTimeout := latencyEnvDuration(t, "DISCOBOX_TERMINAL_LATENCY_TIMEOUT", 5*time.Second)
	loadProfile := envOrDefault("DISCOBOX_TERMINAL_LATENCY_LOAD_PROFILE", "quiet")
	loadHz := latencyEnvNonNegativeInt(t, "DISCOBOX_TERMINAL_LATENCY_LOAD_HZ", 0)
	loadFrameBytes := latencyEnvNonNegativeInt(t, "DISCOBOX_TERMINAL_LATENCY_LOAD_BYTES", 0)
	settle := latencyEnvDuration(t, "DISCOBOX_TERMINAL_LATENCY_SETTLE", 250*time.Millisecond)

	app := &App{
		serverURL: serverURL,
		projectID: projectID,
		token:     strings.TrimSpace(os.Getenv("DISCOBOX_TOKEN")),
		noStart:   true,
		errOut:    io.Discard,
	}

	annotations := newLatencyAnnotationRecorder()
	ctx, cancel := context.WithTimeout(resume.WithObserver(t.Context(), annotations), time.Duration(samples)*sampleTimeout+2*time.Minute)
	defer cancel()

	// No wait for the primary terminal first: the attach waits for it, through
	// the same tiers a real attach does (ADR 0039).
	conn, err := app.openReconnectingSandboxExecAttach(ctx, projectID, sandboxID, primaryExecID, execAttachOptions{replay: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	output := newLatencyOutput()
	readErr := make(chan error, 1)
	go func() {
		for {
			next, readFrameErr := conn.ReadFrame()
			if readFrameErr != nil {
				readErr <- readFrameErr
				return
			}
			switch next.Type {
			case frame.Stdout:
				output.append(next.Payload)
			case frame.Stderr:
				output.append(next.Payload)
			case frame.Error:
				readErr <- fmt.Errorf("remote error: %s", next.Payload)
				return
			case frame.Exit:
				readErr <- fmt.Errorf("latency probe exited: %s", next.Payload)
				return
			}
		}
	}()

	resize, err := frame.EncodeResize(120, 40)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(frame.Resize, resize); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteFrame(frame.Ready, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := app.startSandboxExec(ctx, projectID, sandboxID, primaryExecID); err != nil {
		t.Fatal(err)
	}
	if err := waitLatencyMarker(ctx, output, readErr, latencyReadyMarker, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if settle > 0 {
		time.Sleep(settle)
	}

	report := terminalTransportLatencyReport{
		Kind:           "transport",
		StartedAt:      time.Now().UTC(),
		Server:         serverURL,
		Project:        projectID,
		Sandbox:        sandboxID,
		Samples:        make([]terminalTransportLatencySample, 0, samples),
		SequenceStart:  sequenceStart,
		IntervalMS:     float64(interval) / float64(time.Millisecond),
		LoadProfile:    loadProfile,
		LoadHz:         loadHz,
		LoadFrameBytes: loadFrameBytes,
		SettleMS:       float64(settle) / float64(time.Millisecond),
	}
	outputBytesAtStart := output.bytes()
	measurementStarted := time.Now()
	var lastPosition uint64
	for sample := 0; sample < samples; sample++ {
		seq := sequenceStart + sample
		request := fmt.Sprintf("DBXPING:%08d\r", seq)
		response := fmt.Sprintf("DBXPONG:%08d", seq)

		sentAt := time.Now()
		if err := conn.WriteFrame(frame.Input, []byte(request)); err != nil {
			t.Fatalf("sample %d write: %v", seq, err)
		}
		writeReturnedAt := time.Now()

		accepted, err := annotations.waitNext(ctx, resume.ActionAccepted, frame.Input, lastPosition, sampleTimeout)
		if err != nil {
			t.Fatalf("sample %d acceptance: %v", seq, err)
		}
		lastPosition = accepted.Position
		physical, err := annotations.waitPosition(ctx, resume.ActionPhysicalWrite, accepted.Position, sampleTimeout)
		if err != nil {
			t.Fatalf("sample %d physical write: %v", seq, err)
		}
		if physical.Err != nil {
			t.Fatalf("sample %d physical write: %v", seq, physical.Err)
		}
		if err := waitLatencyMarker(ctx, output, readErr, response, sampleTimeout); err != nil {
			t.Fatalf("sample %d echo: %v", seq, err)
		}
		echoAt := time.Now()
		acknowledged, err := annotations.waitPosition(ctx, resume.ActionAcknowledged, accepted.Position, sampleTimeout)
		if err != nil {
			t.Fatalf("sample %d acknowledgement: %v", seq, err)
		}

		report.Samples = append(report.Samples, terminalTransportLatencySample{
			Sequence:           seq,
			ActionPosition:     accepted.Position,
			WriteCallUS:        durationMicros(writeReturnedAt.Sub(sentAt)),
			PhysicalWriteUS:    durationMicros(physical.Duration),
			ApplyRoundTripUS:   durationMicros(acknowledged.At.Sub(accepted.At)),
			EchoRoundTripUS:    durationMicros(echoAt.Sub(sentAt)),
			PendingBytesAtSend: accepted.PendingBytes,
		})
		if interval > 0 && sample != samples-1 {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(interval):
			}
		}
	}
	measurementDuration := time.Since(measurementStarted)
	report.OutputBytes = output.bytes() - outputBytesAtStart
	report.OutputBytesPerSecond = float64(report.OutputBytes) / measurementDuration.Seconds()
	report.FinishedAt = time.Now().UTC()
	report.Summary = summarizeTransportSamples(report.Samples)

	t.Logf(
		"terminal transport latency: count=%d p50=%.3fms p95=%.3fms p99=%.3fms max=%.3fms; apply-ack p95=%.3fms",
		report.Summary.Count,
		report.Summary.EchoRoundTrip.P50US/1000,
		report.Summary.EchoRoundTrip.P95US/1000,
		report.Summary.EchoRoundTrip.P99US/1000,
		report.Summary.EchoRoundTrip.MaxUS/1000,
		report.Summary.ApplyRoundTrip.P95US/1000,
	)
	if path := strings.TrimSpace(os.Getenv("DISCOBOX_TERMINAL_LATENCY_REPORT")); path != "" {
		if err := writeTerminalLatencyJSON(path, report); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
	}
}

type latencyAnnotationRecorder struct {
	mu      sync.Mutex
	events  map[resume.ActionPhase]map[uint64]resume.ActionEvent
	changed chan struct{}
}

func newLatencyAnnotationRecorder() *latencyAnnotationRecorder {
	return &latencyAnnotationRecorder{
		events:  map[resume.ActionPhase]map[uint64]resume.ActionEvent{},
		changed: make(chan struct{}),
	}
}

func (r *latencyAnnotationRecorder) ObserveAction(event resume.ActionEvent) {
	r.mu.Lock()
	byPosition := r.events[event.Phase]
	if byPosition == nil {
		byPosition = map[uint64]resume.ActionEvent{}
		r.events[event.Phase] = byPosition
	}
	byPosition[event.Position] = event
	close(r.changed)
	r.changed = make(chan struct{})
	r.mu.Unlock()
}

func (r *latencyAnnotationRecorder) waitNext(ctx context.Context, phase resume.ActionPhase, typ byte, after uint64, timeout time.Duration) (resume.ActionEvent, error) {
	return r.wait(ctx, timeout, func(events map[uint64]resume.ActionEvent) (resume.ActionEvent, bool) {
		var position uint64
		for candidate, event := range events {
			if candidate > after && event.Type == typ && (position == 0 || candidate < position) {
				position = candidate
			}
		}
		event, ok := events[position]
		return event, ok
	}, phase)
}

func TestLatencyAnnotationRecorderFiltersActionType(t *testing.T) {
	recorder := newLatencyAnnotationRecorder()
	recorder.ObserveAction(resume.ActionEvent{Phase: resume.ActionAccepted, Position: 1, Type: frame.Resize})
	recorder.ObserveAction(resume.ActionEvent{Phase: resume.ActionAccepted, Position: 2, Type: frame.Input})

	got, err := recorder.waitNext(t.Context(), resume.ActionAccepted, frame.Input, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Position != 2 {
		t.Fatalf("input position = %d, want 2", got.Position)
	}
}

func (r *latencyAnnotationRecorder) waitPosition(ctx context.Context, phase resume.ActionPhase, position uint64, timeout time.Duration) (resume.ActionEvent, error) {
	return r.wait(ctx, timeout, func(events map[uint64]resume.ActionEvent) (resume.ActionEvent, bool) {
		event, ok := events[position]
		return event, ok
	}, phase)
}

func (r *latencyAnnotationRecorder) wait(
	ctx context.Context,
	timeout time.Duration,
	find func(map[uint64]resume.ActionEvent) (resume.ActionEvent, bool),
	phase resume.ActionPhase,
) (resume.ActionEvent, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		r.mu.Lock()
		if event, ok := find(r.events[phase]); ok {
			r.mu.Unlock()
			return event, nil
		}
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return resume.ActionEvent{}, ctx.Err()
		case <-timer.C:
			return resume.ActionEvent{}, fmt.Errorf("timed out waiting for %s annotation", phase)
		case <-changed:
		}
	}
}

type latencyOutput struct {
	mu      sync.Mutex
	data    []byte
	total   uint64
	changed chan struct{}
}

func newLatencyOutput() *latencyOutput {
	return &latencyOutput{changed: make(chan struct{})}
}

func (o *latencyOutput) append(payload []byte) {
	o.mu.Lock()
	o.total += uint64(len(payload))
	o.data = append(o.data, payload...)
	if len(o.data) > 4*1024*1024 {
		copy(o.data, o.data[len(o.data)-2*1024*1024:])
		o.data = o.data[:2*1024*1024]
	}
	close(o.changed)
	o.changed = make(chan struct{})
	o.mu.Unlock()
}

func (o *latencyOutput) bytes() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.total
}

func (o *latencyOutput) wait(ctx context.Context, marker string, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	needle := []byte(marker)
	for {
		o.mu.Lock()
		if bytes.Contains(o.data, needle) {
			o.mu.Unlock()
			return nil
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("timed out waiting for output marker %q", marker)
		case <-changed:
		}
	}
}

func waitLatencyMarker(ctx context.Context, output *latencyOutput, readErr <-chan error, marker string, timeout time.Duration) error {
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- output.wait(waitCtx, marker, timeout) }()
	select {
	case err := <-readErr:
		cancel()
		if err == nil {
			return io.EOF
		}
		return err
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type terminalTransportLatencySample struct {
	Sequence           int     `json:"sequence"`
	ActionPosition     uint64  `json:"actionPosition"`
	WriteCallUS        float64 `json:"writeCallUs"`
	PhysicalWriteUS    float64 `json:"physicalWriteUs"`
	ApplyRoundTripUS   float64 `json:"applyRoundTripUs"`
	EchoRoundTripUS    float64 `json:"echoRoundTripUs"`
	PendingBytesAtSend int     `json:"pendingBytesAtSend"`
}

type terminalTransportLatencyReport struct {
	Kind                 string                           `json:"kind"`
	StartedAt            time.Time                        `json:"startedAt"`
	FinishedAt           time.Time                        `json:"finishedAt"`
	Server               string                           `json:"server"`
	Project              string                           `json:"project"`
	Sandbox              string                           `json:"sandbox"`
	SequenceStart        int                              `json:"sequenceStart"`
	IntervalMS           float64                          `json:"intervalMs"`
	LoadProfile          string                           `json:"loadProfile"`
	LoadHz               int                              `json:"loadHz"`
	LoadFrameBytes       int                              `json:"loadFrameBytes"`
	SettleMS             float64                          `json:"settleMs"`
	OutputBytes          uint64                           `json:"outputBytes"`
	OutputBytesPerSecond float64                          `json:"outputBytesPerSecond"`
	Samples              []terminalTransportLatencySample `json:"samples"`
	Summary              terminalTransportLatencySummary  `json:"summary"`
}

type terminalTransportLatencySummary struct {
	Count          int                    `json:"count"`
	WriteCall      terminalLatencySummary `json:"writeCall"`
	PhysicalWrite  terminalLatencySummary `json:"physicalWrite"`
	ApplyRoundTrip terminalLatencySummary `json:"applyRoundTrip"`
	EchoRoundTrip  terminalLatencySummary `json:"echoRoundTrip"`
}

type terminalLatencySummary struct {
	MinUS  float64 `json:"minUs"`
	MeanUS float64 `json:"meanUs"`
	P50US  float64 `json:"p50Us"`
	P90US  float64 `json:"p90Us"`
	P95US  float64 `json:"p95Us"`
	P99US  float64 `json:"p99Us"`
	MaxUS  float64 `json:"maxUs"`
}

func summarizeTransportSamples(samples []terminalTransportLatencySample) terminalTransportLatencySummary {
	writeCalls := make([]float64, 0, len(samples))
	physicalWrites := make([]float64, 0, len(samples))
	applyRoundTrips := make([]float64, 0, len(samples))
	echoRoundTrips := make([]float64, 0, len(samples))
	for _, sample := range samples {
		writeCalls = append(writeCalls, sample.WriteCallUS)
		physicalWrites = append(physicalWrites, sample.PhysicalWriteUS)
		applyRoundTrips = append(applyRoundTrips, sample.ApplyRoundTripUS)
		echoRoundTrips = append(echoRoundTrips, sample.EchoRoundTripUS)
	}
	return terminalTransportLatencySummary{
		Count:          len(samples),
		WriteCall:      summarizeTerminalLatencies(writeCalls),
		PhysicalWrite:  summarizeTerminalLatencies(physicalWrites),
		ApplyRoundTrip: summarizeTerminalLatencies(applyRoundTrips),
		EchoRoundTrip:  summarizeTerminalLatencies(echoRoundTrips),
	}
}

func summarizeTerminalLatencies(values []float64) terminalLatencySummary {
	if len(values) == 0 {
		return terminalLatencySummary{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	var sum float64
	for _, value := range sorted {
		sum += value
	}
	return terminalLatencySummary{
		MinUS:  sorted[0],
		MeanUS: sum / float64(len(sorted)),
		P50US:  latencyPercentile(sorted, 0.50),
		P90US:  latencyPercentile(sorted, 0.90),
		P95US:  latencyPercentile(sorted, 0.95),
		P99US:  latencyPercentile(sorted, 0.99),
		MaxUS:  sorted[len(sorted)-1],
	}
}

func latencyPercentile(sorted []float64, quantile float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func durationMicros(value time.Duration) float64 {
	return float64(value) / float64(time.Microsecond)
}

func latencyEnvInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		t.Fatalf("%s must be a positive integer", key)
	}
	return parsed
}

func latencyEnvNonNegativeInt(t *testing.T, key string, fallback int) int {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		t.Fatalf("%s must be a non-negative integer", key)
	}
	return parsed
}

func latencyEnvDuration(t *testing.T, key string, fallback time.Duration) time.Duration {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		t.Fatalf("%s must be a non-negative duration", key)
	}
	return parsed
}

func writeTerminalLatencyJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

var _ resume.Observer = (*latencyAnnotationRecorder)(nil)
