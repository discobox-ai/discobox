package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
)

const readyMarker = "DISCOBOX_LATENCY_READY"

type options struct {
	server        string
	project       string
	sandbox       string
	sandboxName   string
	token         string
	cli           string
	mode          string
	samples       int
	sequenceStart int
	interval      time.Duration
	timeout       time.Duration
	output        string
	container     string
	tmuxWidth     int
	tmuxHeight    int
	loadProfile   string
	loadHz        int
	loadBytes     int
	settle        time.Duration
}

func main() {
	var opts options
	flag.StringVar(&opts.server, "server", "", "Discobox server endpoint (default: the endpoint discobox dials on its own)")
	flag.StringVar(&opts.project, "project", "default", "Discobox project")
	flag.StringVar(&opts.sandbox, "sandbox", "", "sandbox ID running the terminal-latency harness")
	flag.StringVar(&opts.sandboxName, "sandbox-name", "", "sandbox name shown in the TUI")
	flag.StringVar(&opts.token, "token", os.Getenv("DISCOBOX_TOKEN"), "optional Discobox bearer token")
	flag.StringVar(&opts.cli, "cli", "build/discobox", "path to the discobox CLI")
	flag.StringVar(&opts.mode, "mode", "direct", "client path to measure: direct or tui")
	flag.IntVar(&opts.samples, "samples", 100, "number of request/response samples")
	flag.IntVar(&opts.sequenceStart, "sequence-start", 1, "first eight-digit probe sequence")
	flag.DurationVar(&opts.interval, "interval", 20*time.Millisecond, "delay between samples")
	flag.DurationVar(&opts.timeout, "timeout", 5*time.Second, "timeout for one response")
	flag.StringVar(&opts.output, "output", "", "JSON report path; stdout only when empty")
	flag.StringVar(&opts.container, "container", "", "optional sandbox Docker container ID for cgroup snapshots")
	flag.IntVar(&opts.tmuxWidth, "width", 120, "tmux pane width")
	flag.IntVar(&opts.tmuxHeight, "height", 40, "tmux pane height")
	flag.StringVar(&opts.loadProfile, "load-profile", "quiet", "sandbox output profile: quiet, spinner, or screen")
	flag.IntVar(&opts.loadHz, "load-hz", 0, "configured sandbox output writes per second")
	flag.IntVar(&opts.loadBytes, "load-bytes", 0, "configured bytes per sandbox output write")
	flag.DurationVar(&opts.settle, "settle", 250*time.Millisecond, "delay after attach readiness before sampling")
	flag.Parse()

	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "terminal latency:", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	// An unset endpoint resolves here rather than at the flag, so the empty
	// value run.sh passes when it was told nothing means the same thing as
	// omitting the flag, and the report records the endpoint that was used.
	if strings.TrimSpace(opts.server) == "" {
		opts.server = endpoint.DefaultEndpoint()
	}
	if strings.TrimSpace(opts.sandbox) == "" {
		return errors.New("--sandbox is required")
	}
	if opts.samples < 1 {
		return errors.New("--samples must be positive")
	}
	if opts.sequenceStart < 1 || opts.sequenceStart+opts.samples-1 > 99_999_999 {
		return errors.New("--sequence-start and --samples must fit the eight-digit probe protocol")
	}
	if opts.interval < 0 || opts.timeout <= 0 || opts.settle < 0 {
		return errors.New("--interval and --settle must be non-negative and --timeout must be positive")
	}
	switch opts.loadProfile {
	case "quiet", "spinner", "screen":
	default:
		return fmt.Errorf("--load-profile must be quiet, spinner, or screen, got %q", opts.loadProfile)
	}
	if opts.loadHz < 0 || opts.loadBytes < 0 {
		return errors.New("--load-hz and --load-bytes must be non-negative")
	}
	if opts.loadProfile != "quiet" && (opts.loadHz == 0 || opts.loadBytes == 0) {
		return errors.New("--load-hz and --load-bytes must be positive for a loaded profile")
	}
	if opts.mode != "direct" && opts.mode != "tui" {
		return fmt.Errorf("--mode must be direct or tui, got %q", opts.mode)
	}
	if opts.mode == "tui" && strings.TrimSpace(opts.sandboxName) == "" {
		return errors.New("--sandbox-name is required in tui mode")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return errors.New("tmux is required")
	}
	cliPath, err := filepath.Abs(opts.cli)
	if err != nil {
		return err
	}
	if _, err := os.Stat(cliPath); err != nil {
		return fmt.Errorf("CLI %s: %w", cliPath, err)
	}

	report := tmuxLatencyReport{
		Kind:           opts.mode,
		Measurement:    "tmux control-mode input enqueue to sequence-tagged pane output",
		StartedAt:      time.Now().UTC(),
		Server:         opts.server,
		Project:        opts.project,
		Sandbox:        opts.sandbox,
		SandboxName:    opts.sandboxName,
		SequenceStart:  opts.sequenceStart,
		IntervalMS:     durationMillis(opts.interval),
		TmuxVersion:    commandOutput("tmux", "-V"),
		LoadProfile:    opts.loadProfile,
		LoadHz:         opts.loadHz,
		LoadFrameBytes: opts.loadBytes,
		SettleMS:       durationMillis(opts.settle),
		Samples:        make([]tmuxLatencySample, 0, opts.samples),
	}

	command := []string{cliPath, "--server", opts.server, "--project", opts.project, "--no-start"}
	if opts.token != "" {
		command = append(command, "--token", opts.token)
	}
	switch opts.mode {
	case "direct":
		command = append(command, "admin", "terminal", "--discobox-id", opts.sandbox, "attach", "primary")
	case "tui":
		command = append(command, "tui")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller, err := startTmux(ctx, command, opts.tmuxWidth, opts.tmuxHeight)
	if err != nil {
		return err
	}
	defer controller.Close()

	if opts.mode == "tui" {
		// Names may be elided to fit the table, while IDs are rendered in full.
		// Waiting for the exact disposable ID also avoids selecting a similarly
		// named pre-existing sandbox.
		if err := controller.WaitOutput(opts.sandbox, 30*time.Second); err != nil {
			return fmt.Errorf("wait for sandbox row: %w", err)
		}
		if err := controller.SendBytes([]byte("G")); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		if err := controller.SendBytes([]byte{'\r'}); err != nil {
			return err
		}
	}
	if err := controller.WaitOutput(readyMarker, 45*time.Second); err != nil {
		return fmt.Errorf("wait for latency probe: %w", err)
	}
	if opts.settle > 0 {
		time.Sleep(opts.settle)
	}

	report.Before = captureSystemSnapshot(opts.container)
	outputBytesAtStart := controller.OutputBytes()
	measurementStarted := time.Now()
	for sample := 0; sample < opts.samples; sample++ {
		seq := opts.sequenceStart + sample
		request := fmt.Sprintf("DBXPING:%08d\r", seq)
		response := fmt.Sprintf("DBXPONG:%08d", seq)
		sentAt := time.Now()
		if err := controller.SendBytes([]byte(request)); err != nil {
			return fmt.Errorf("sample %d send: %w", seq, err)
		}
		if err := controller.WaitOutput(response, opts.timeout); err != nil {
			return fmt.Errorf("sample %d response: %w", seq, err)
		}
		report.Samples = append(report.Samples, tmuxLatencySample{
			Sequence:    seq,
			RoundTripUS: durationMicros(time.Since(sentAt)),
		})
		if opts.interval > 0 && sample != opts.samples-1 {
			time.Sleep(opts.interval)
		}
	}
	measurementDuration := time.Since(measurementStarted)
	report.PaneOutputBytes = controller.OutputBytes() - outputBytesAtStart
	report.PaneOutputBytesPerSecond = float64(report.PaneOutputBytes) / measurementDuration.Seconds()

	report.FinishedAt = time.Now().UTC()
	report.After = captureSystemSnapshot(opts.container)
	report.CgroupCPUStatDelta = diffUint64Maps(report.Before.SandboxCPUStat(), report.After.SandboxCPUStat())
	values := make([]float64, 0, len(report.Samples))
	for _, sample := range report.Samples {
		values = append(values, sample.RoundTripUS)
	}
	report.Summary = summarize(values)

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if opts.output != "" {
		if err := os.MkdirAll(filepath.Dir(opts.output), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(opts.output, append(data, '\n'), 0o600); err != nil {
			return err
		}
		fmt.Printf(
			"%s/%s latency: count=%d p50=%.3fms p95=%.3fms p99=%.3fms max=%.3fms output=%.1fKiB/s; report=%s\n",
			opts.mode,
			opts.loadProfile,
			report.Summary.Count,
			report.Summary.P50US/1000,
			report.Summary.P95US/1000,
			report.Summary.P99US/1000,
			report.Summary.MaxUS/1000,
			report.PaneOutputBytesPerSecond/1024,
			opts.output,
		)
		return nil
	}
	fmt.Println(string(data))
	return nil
}

type tmuxController struct {
	socket  string
	session string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	output  *streamBuffer
	stderr  lockedBuffer
	done    chan error
	bytes   atomic.Uint64
	mu      sync.Mutex
	closed  bool
}

func startTmux(ctx context.Context, command []string, width, height int) (*tmuxController, error) {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	socket := "dbx-terminal-perf-" + suffix
	session := "dbx-terminal-perf-" + suffix
	shellCommand := "exec " + shellJoin(command)
	cmd := exec.CommandContext(
		ctx,
		"tmux", "-L", socket, "-C",
		"new-session", "-s", session,
		"-x", strconv.Itoa(width), "-y", strconv.Itoa(height),
		shellCommand,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	controller := &tmuxController{
		socket:  socket,
		session: session,
		cmd:     cmd,
		stdin:   stdin,
		output:  newStreamBuffer(8 * 1024 * 1024),
		done:    make(chan error, 1),
	}
	cmd.Stderr = &controller.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go controller.readControlOutput(stdout)
	go func() {
		err := cmd.Wait()
		controller.output.finish(err)
		controller.done <- err
	}()
	return controller, nil
}

func (c *tmuxController) readControlOutput(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := tmuxOutputPayload(line)
		if ok {
			c.bytes.Add(uint64(len(payload)))
			c.output.append(payload)
		}
	}
	if err := scanner.Err(); err != nil {
		c.output.finish(err)
	}
}

func (c *tmuxController) OutputBytes() uint64 {
	return c.bytes.Load()
}

func (c *tmuxController) SendBytes(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	var command strings.Builder
	fmt.Fprintf(&command, "send-keys -t %s:0.0 -H", c.session)
	for _, value := range payload {
		fmt.Fprintf(&command, " %02x", value)
	}
	command.WriteByte('\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("tmux controller is closed")
	}
	if _, err := io.WriteString(c.stdin, command.String()); err != nil {
		return fmt.Errorf("send tmux keys: %w", err)
	}
	return nil
}

func (c *tmuxController) WaitOutput(marker string, timeout time.Duration) error {
	if err := c.output.wait([]byte(marker), timeout); err != nil {
		stderr := strings.TrimSpace(c.stderr.String())
		if stderr != "" {
			return fmt.Errorf("%w; tmux: %s", err, stderr)
		}
		return err
	}
	return nil
}

func (c *tmuxController) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	_, _ = io.WriteString(c.stdin, "kill-server\n")
	_ = c.stdin.Close()
	c.mu.Unlock()

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "tmux", "-L", c.socket, "kill-server").Run() //nolint:gosec // The socket name is generated locally, not from caller input.
	}
}

func tmuxOutputPayload(line string) ([]byte, bool) {
	if strings.HasPrefix(line, "%output ") {
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return nil, false
		}
		return decodeTmuxEscapes(parts[2]), true
	}
	if !strings.HasPrefix(line, "%extended-output ") {
		return nil, false
	}
	// Extended output inserts an age and reserved fields between the pane ID
	// and a single ":" delimiter. They are metadata, not pane bytes.
	_, payload, ok := strings.Cut(line, " : ")
	if !ok {
		return nil, false
	}
	return decodeTmuxEscapes(payload), true
}

func decodeTmuxEscapes(value string) []byte {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			out = append(out, value[i])
			continue
		}
		if i+3 < len(value) && isOctal(value[i+1]) && isOctal(value[i+2]) && isOctal(value[i+3]) {
			decoded := (value[i+1]-'0')*64 + (value[i+2]-'0')*8 + value[i+3] - '0'
			out = append(out, decoded)
			i += 3
			continue
		}
		if i+1 < len(value) {
			i++
			out = append(out, value[i])
			continue
		}
		out = append(out, '\\')
	}
	return out
}

func isOctal(value byte) bool { return value >= '0' && value <= '7' }

type streamBuffer struct {
	mu      sync.Mutex
	data    []byte
	limit   int
	changed chan struct{}
	done    bool
	err     error
}

type lockedBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *lockedBuffer) Write(payload []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(payload)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.String()
}

func newStreamBuffer(limit int) *streamBuffer {
	return &streamBuffer{limit: limit, changed: make(chan struct{})}
}

func (b *streamBuffer) append(payload []byte) {
	b.mu.Lock()
	b.data = append(b.data, payload...)
	if len(b.data) > b.limit {
		keep := b.limit / 2
		copy(b.data, b.data[len(b.data)-keep:])
		b.data = b.data[:keep]
	}
	b.notifyLocked()
	b.mu.Unlock()
}

func (b *streamBuffer) finish(err error) {
	b.mu.Lock()
	if !b.done {
		b.done = true
		b.err = err
		b.notifyLocked()
	}
	b.mu.Unlock()
}

func (b *streamBuffer) wait(marker []byte, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		b.mu.Lock()
		if bytes.Contains(b.data, marker) {
			b.mu.Unlock()
			return nil
		}
		if b.done {
			err := b.err
			b.mu.Unlock()
			if err == nil {
				return io.EOF
			}
			return err
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-timer.C:
			return fmt.Errorf("timed out waiting for %q", marker)
		case <-changed:
		}
	}
}

func (b *streamBuffer) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

type tmuxLatencySample struct {
	Sequence    int     `json:"sequence"`
	RoundTripUS float64 `json:"roundTripUs"`
}

type tmuxLatencyReport struct {
	Kind                     string              `json:"kind"`
	Measurement              string              `json:"measurement"`
	StartedAt                time.Time           `json:"startedAt"`
	FinishedAt               time.Time           `json:"finishedAt"`
	Server                   string              `json:"server"`
	Project                  string              `json:"project"`
	Sandbox                  string              `json:"sandbox"`
	SandboxName              string              `json:"sandboxName,omitempty"`
	SequenceStart            int                 `json:"sequenceStart"`
	IntervalMS               float64             `json:"intervalMs"`
	TmuxVersion              string              `json:"tmuxVersion"`
	LoadProfile              string              `json:"loadProfile"`
	LoadHz                   int                 `json:"loadHz"`
	LoadFrameBytes           int                 `json:"loadFrameBytes"`
	SettleMS                 float64             `json:"settleMs"`
	PaneOutputBytes          uint64              `json:"paneOutputBytes"`
	PaneOutputBytesPerSecond float64             `json:"paneOutputBytesPerSecond"`
	Before                   systemSnapshot      `json:"before"`
	After                    systemSnapshot      `json:"after"`
	CgroupCPUStatDelta       map[string]uint64   `json:"cgroupCpuStatDelta,omitempty"`
	Samples                  []tmuxLatencySample `json:"samples"`
	Summary                  latencySummary      `json:"summary"`
}

type latencySummary struct {
	Count  int     `json:"count"`
	MinUS  float64 `json:"minUs"`
	MeanUS float64 `json:"meanUs"`
	P50US  float64 `json:"p50Us"`
	P90US  float64 `json:"p90Us"`
	P95US  float64 `json:"p95Us"`
	P99US  float64 `json:"p99Us"`
	MaxUS  float64 `json:"maxUs"`
}

func summarize(values []float64) latencySummary {
	if len(values) == 0 {
		return latencySummary{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	var sum float64
	for _, value := range sorted {
		sum += value
	}
	return latencySummary{
		Count:  len(sorted),
		MinUS:  sorted[0],
		MeanUS: sum / float64(len(sorted)),
		P50US:  percentile(sorted, 0.50),
		P90US:  percentile(sorted, 0.90),
		P95US:  percentile(sorted, 0.95),
		P99US:  percentile(sorted, 0.99),
		MaxUS:  sorted[len(sorted)-1],
	}
}

func percentile(sorted []float64, quantile float64) float64 {
	index := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

type systemSnapshot struct {
	At            time.Time       `json:"at"`
	LoadAverage   string          `json:"loadAverage,omitempty"`
	HostCPU       string          `json:"hostCpuPressure,omitempty"`
	HostIO        string          `json:"hostIoPressure,omitempty"`
	HostMemory    string          `json:"hostMemoryPressure,omitempty"`
	SandboxCgroup *cgroupSnapshot `json:"sandboxCgroup,omitempty"`
}

func (s systemSnapshot) SandboxCPUStat() map[string]uint64 {
	if s.SandboxCgroup == nil {
		return nil
	}
	return s.SandboxCgroup.CPUStat
}

type cgroupSnapshot struct {
	Path        string            `json:"path"`
	CPUMax      string            `json:"cpuMax,omitempty"`
	CPUPressure string            `json:"cpuPressure,omitempty"`
	CPUStat     map[string]uint64 `json:"cpuStat,omitempty"`
	Memory      string            `json:"memoryCurrent,omitempty"`
}

func captureSystemSnapshot(container string) systemSnapshot {
	snapshot := systemSnapshot{
		At:          time.Now().UTC(),
		LoadAverage: readTrimmed("/proc/loadavg"),
		HostCPU:     readTrimmed("/proc/pressure/cpu"),
		HostIO:      readTrimmed("/proc/pressure/io"),
		HostMemory:  readTrimmed("/proc/pressure/memory"),
	}
	if container != "" {
		snapshot.SandboxCgroup = captureContainerCgroup(container)
	}
	return snapshot
}

func captureContainerCgroup(container string) *cgroupSnapshot {
	pidText := commandOutput("docker", "inspect", "--format", "{{.State.Pid}}", container)
	pid, err := strconv.Atoi(strings.TrimSpace(pidText))
	if err != nil || pid <= 0 {
		return nil
	}
	cgroupData, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return nil
	}
	var relative string
	for _, line := range strings.Split(string(cgroupData), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) == 3 && parts[0] == "0" {
			relative = strings.TrimSpace(parts[2])
			break
		}
	}
	if relative == "" {
		return nil
	}
	root := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(relative, "/"))
	for {
		cpuMax := readTrimmed(filepath.Join(root, "cpu.max"))
		if cpuMax != "" && !strings.HasPrefix(cpuMax, "max ") {
			return &cgroupSnapshot{
				Path:        root,
				CPUMax:      cpuMax,
				CPUPressure: readTrimmed(filepath.Join(root, "cpu.pressure")),
				CPUStat:     readUint64Fields(filepath.Join(root, "cpu.stat")),
				Memory:      readTrimmed(filepath.Join(root, "memory.current")),
			}
		}
		parent := filepath.Dir(root)
		if parent == root || !strings.HasPrefix(parent, "/sys/fs/cgroup") {
			break
		}
		root = parent
	}
	return nil
}

func readUint64Fields(path string) map[string]uint64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			out[fields[0]] = value
		}
	}
	return out
}

func diffUint64Maps(before, after map[string]uint64) map[string]uint64 {
	if len(before) == 0 || len(after) == 0 {
		return nil
	}
	out := map[string]uint64{}
	for key, afterValue := range after {
		beforeValue, ok := before[key]
		if ok && afterValue >= beforeValue {
			out[key] = afterValue - beforeValue
		}
	}
	return out
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func commandOutput(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func durationMicros(value time.Duration) float64 {
	return float64(value) / float64(time.Microsecond)
}

func durationMillis(value time.Duration) float64 {
	return float64(value) / float64(time.Millisecond)
}
