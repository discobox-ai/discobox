package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/health"
	"github.com/discobox-ai/discobox/version"
)

// statusLayerAPI is the one layer this command adds above the transport: an
// authenticated call to the API the rest of the CLI uses. Everything below it
// is the endpoint package's, because everything below it is the endpoint
// package's to see.
const statusLayerAPI = "api"

// statusReport is what `discobox status` prints and what -o json emits. It is
// the transport's diagnosis plus the two things only this side knows: which
// client asked, and whether the API answered it.
type statusReport struct {
	Client    statusClient       `json:"client"`
	Endpoint  endpoint.Diagnosis `json:"endpoint"`
	Reachable bool               `json:"reachable"`
}

type statusClient struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
	// IdentityFile is where this machine's peer ID is kept, named only for an
	// endpoint that uses one. It is the file an operator backs up, moves, or
	// deletes to become a different peer, and nothing else prints it.
	IdentityFile string `json:"identityFile,omitempty"`
}

// newStatusCommand reports whether this client can reach its server, and where
// it stops when it cannot.
//
// It exists because a failure to reach a server over iroh arrives as one line
// naming the top of a stack — the dial failed — when the thing to fix is
// somewhere underneath it: a library that did not load, a relay this network
// cannot reach, a peer ID that names a server that is switched off, or a
// server that is running and does not admit this machine. Those have four
// different fixes and, until this command, one error message.
func (a *App) newStatusCommand() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check the connection to the server, layer by layer",
		Long: "Check the connection to the server and report where it stops.\n\n" +
			"Each layer is reported separately — the address, this machine's identity, the\n" +
			"socket, the relay, the connection to the peer, whether the server admits this\n" +
			"machine, whether it is ready, and the route traffic ends up taking — so a\n" +
			"failure names the layer to fix rather than the one on top of it.\n\n" +
			"This never starts a server: it reports what is there. For a running account of\n" +
			"the same layers while another command connects, use --iroh-log=debug.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatus(cmd, timeout)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 45*time.Second, "How long to spend on the whole check")
	return cmd
}

func (a *App) runStatus(cmd *cobra.Command, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	report := statusReport{Client: statusClient{
		Version:  version.String(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}}

	parsed := a.serverEndpoint()
	if parsed.Scheme == "iroh" {
		report.Client.IdentityFile = defaultIrohIdentityPath()
		// The same call every other command makes, so what is diagnosed is the
		// identity and the relays a real connection would use. A failure here
		// is the address layer's answer rather than the command's error: this
		// command's whole job is to report a broken connection instead of
		// exiting on one.
		if err := configureIrohForEndpoint(parsed, a.irohRelayURLs, a.irohLogLevel); err != nil {
			report.Endpoint = endpoint.Diagnosis{
				Endpoint:  a.serverURL,
				Scheme:    parsed.Scheme,
				Transport: "iroh",
				Steps: []endpoint.DiagnosisStep{{
					Layer:   endpoint.DiagnosisLayerIdentity,
					Status:  endpoint.DiagnosisFailed,
					Summary: "this machine's identity could not be prepared",
					Error:   err.Error(),
				}},
			}
			return a.writeStatus(cmd, report)
		}
	}

	report.Endpoint = endpoint.Diagnose(ctx, a.serverURL, endpoint.DiagnoseOptions{})
	report.Endpoint.Steps = append(report.Endpoint.Steps, a.statusAPILayer(ctx, report.Endpoint))
	report.Reachable = report.Endpoint.OK()
	return a.writeStatus(cmd, report)
}

// statusAPILayer decides whether to make the API call at all, and skips it in
// the two cases where making it would report the wrong thing.
//
// A server that is still starting is the one that matters: it answers *every*
// path with 503 until its real router is serving (server/internal/server's
// startupHandler), so probing it produces a failed api layer, a non-zero exit,
// and a hint about tokens — directly underneath a row that says "starting".
// The transport got all the way there and the server said what is wrong with
// it; there is nothing for this layer to add.
func (a *App) statusAPILayer(ctx context.Context, diagnosis endpoint.Diagnosis) endpoint.DiagnosisStep {
	switch {
	case !diagnosis.OK():
		return endpoint.DiagnosisStep{
			Layer:   statusLayerAPI,
			Status:  endpoint.DiagnosisSkipped,
			Summary: "not reached",
		}
	case diagnosis.ServerStatus == health.StatusStarting:
		return endpoint.DiagnosisStep{
			Layer:   statusLayerAPI,
			Status:  endpoint.DiagnosisSkipped,
			Summary: "not asked: the server is still starting, and answers every request with 503 until it is ready",
		}
	default:
		// Timed here rather than inside the call, and stamped on the value
		// about to be returned. A duration set from a defer inside a function
		// with an unnamed result is written to a copy nobody sees, which is
		// how this row spent its first version reporting no elapsed time at
		// all.
		started := time.Now()
		step := a.statusAPIStep(ctx)
		step.DurationMS = time.Since(started).Milliseconds()
		return step
	}
}

// statusAPIStep is the layer above the transport: an authenticated request the
// generated client makes, which is what every other command does and the only
// thing that proves the connection is usable rather than merely open.
//
// It lists projects because that is the cheapest call every deployment
// answers, and because a server that admits this peer at the transport can
// still refuse the principal it maps to.
// Its caller times it, so every return path here is just an answer.
func (a *App) statusAPIStep(ctx context.Context) endpoint.DiagnosisStep {
	step := endpoint.DiagnosisStep{Layer: statusLayerAPI}

	// No autolaunch, which is why this builds its own client rather than
	// calling apiClient: a status that starts a server answers a question
	// nobody asked and destroys the one that was asked, which is whether a
	// server is there.
	baseURL, httpClient, err := a.httpClientWithAutoStart(false)
	if err != nil {
		step.Status = endpoint.DiagnosisFailed
		step.Summary = "no API client for this endpoint"
		step.Error = err.Error()
		return step
	}
	client, err := apiclientgen.NewClient(baseURL, apiclientgen.WithClient(httpClient))
	if err != nil {
		step.Status = endpoint.DiagnosisFailed
		step.Summary = "no API client for this endpoint"
		step.Error = err.Error()
		return step
	}
	res, err := client.ListProjects(ctx)
	if err != nil {
		step.Status = endpoint.DiagnosisFailed
		step.Summary = "the API did not answer"
		step.Error = err.Error()
		step.Hint = "The transport is up, so this is the server answering. --token, or DISCOBOX_TOKEN, is what an API request authenticates with."
		return step
	}
	body, err := expectResponse[apimodel.ListProjectsBody](res)
	if err != nil {
		step.Status = endpoint.DiagnosisFailed
		step.Summary = "the API refused this request"
		step.Error = err.Error()
		return step
	}
	step.Status = endpoint.DiagnosisOK
	step.Summary = fmt.Sprintf("authenticated · %d %s", len(body.GetProjects()), pluralize("project", len(body.GetProjects())))
	return step
}

func (a *App) writeStatus(cmd *cobra.Command, report statusReport) error {
	if a.output == "json" {
		if err := writeJSON(cmd.OutOrStdout(), report); err != nil {
			return err
		}
		return statusExit(report)
	}
	printStatus(cmd.OutOrStdout(), report)
	return statusExit(report)
}

// statusExit makes an unreachable server a non-zero exit, so `discobox status`
// is usable as a check in a script.
//
// The message is deliberately short and names no layer: the report on stdout
// has already named it and said what to do about it, and an error line
// repeating that would be the same sentence twice on two streams.
func statusExit(report statusReport) error {
	if !report.Reachable {
		return errors.New("the server is not reachable")
	}
	return nil
}

const (
	statusColDim  = "245"
	statusColOK   = "83"
	statusColWarn = "214"
	statusColErr  = "196"
)

var (
	statusStyleDim   = lipgloss.NewStyle().Foreground(lipgloss.Color(statusColDim))
	statusStyleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color(statusColOK)).Bold(true)
	statusStyleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color(statusColWarn)).Bold(true)
	statusStyleErr   = lipgloss.NewStyle().Foreground(lipgloss.Color(statusColErr)).Bold(true)
	statusStyleBold  = lipgloss.NewStyle().Bold(true)
	statusStyleLayer = lipgloss.NewStyle().Bold(true)
)

// printStatus draws the report: a header of what this is and what it is
// dialing, then one row per layer, then the one thing to do about a failure.
//
// The mark and the status word both appear, for the same reason the apply
// report prints both — color is taken away by the writer for a pipe or a file,
// and a report pasted into an issue has to still say which layer failed.
//
// The columns are padded here rather than by a tabwriter, because every cell
// is painted: a tabwriter measures the escape sequences as characters and
// lines the rows up by a width nobody sees.
func printStatus(out io.Writer, report statusReport) {
	writer := colorprofile.NewWriter(out, os.Environ())
	paint := func(style lipgloss.Style, text string) string {
		if text == "" {
			return text
		}
		return style.Render(text)
	}

	fmt.Fprintf(writer, "client    %s %s\n", paint(statusStyleBold, report.Client.Version), paint(statusStyleDim, report.Client.Platform))
	fmt.Fprintf(writer, "server    %s\n", paint(statusStyleBold, report.Endpoint.Endpoint))
	if report.Endpoint.Transport != "" {
		fmt.Fprintf(writer, "transport %s\n", paint(statusStyleDim, report.Endpoint.Transport))
	}
	if report.Client.IdentityFile != "" {
		fmt.Fprintf(writer, "identity  %s\n", paint(statusStyleDim, report.Client.IdentityFile))
	}
	fmt.Fprintln(writer)

	statusWidth, layerWidth := statusColumnWidths(report.Endpoint.Steps)
	// Two spaces of gutter between the columns, and the marker plus its space
	// ahead of them: what a detail line has to clear to sit under the summary.
	indent := strings.Repeat(" ", 2+2+statusWidth+2+layerWidth+2)
	for _, step := range report.Endpoint.Steps {
		mark, style := statusMark(step.Status)
		row := fmt.Sprintf("  %s %s  %s  %s",
			paint(style, mark),
			paint(style, pad(strings.ToUpper(string(step.Status)), statusWidth)),
			paint(statusStyleLayer, pad(step.Layer, layerWidth)),
			step.Summary)
		if elapsed := statusDuration(step); elapsed != "" {
			row += "  " + paint(statusStyleDim, elapsed)
		}
		fmt.Fprintln(writer, row)
		for _, detail := range step.Detail {
			fmt.Fprintf(writer, "%s%s\n", indent, paint(statusStyleDim, detail))
		}
		// The transport's own words, kept under the summary that interprets
		// them: the summary is what this command concluded, and this is what it
		// concluded it from.
		if step.Error != "" && step.Error != step.Summary {
			fmt.Fprintf(writer, "%s%s\n", indent, paint(statusStyleDim, step.Error))
		}
	}

	fmt.Fprintln(writer)
	if failure := report.Endpoint.FirstFailure(); failure != nil {
		fmt.Fprintln(writer, paint(statusStyleErr, "Cannot reach the server: the "+failure.Layer+" layer failed."))
		if failure.Hint != "" {
			fmt.Fprintln(writer, failure.Hint)
		}
		return
	}
	// A warning is a layer that failed without stopping the one above it, so it
	// is reported after the good news rather than instead of it.
	for _, step := range report.Endpoint.Steps {
		if step.Status == endpoint.DiagnosisWarn && step.Hint != "" {
			fmt.Fprintln(writer, paint(statusStyleWarn, step.Layer+": "+step.Hint))
		}
	}
	fmt.Fprintln(writer, paint(statusStyleOK, "The server is reachable."))
}

// statusColumnWidths measures the two fixed columns against this report's own
// rows, so a report with no skipped layers is not padded to the width of the
// word "SKIPPED".
func statusColumnWidths(steps []endpoint.DiagnosisStep) (status, layer int) {
	for _, step := range steps {
		status = max(status, len(step.Status))
		layer = max(layer, len(step.Layer))
	}
	return status, layer
}

func pad(text string, width int) string {
	if len(text) >= width {
		return text
	}
	return text + strings.Repeat(" ", width-len(text))
}

func statusMark(status endpoint.DiagnosisStatus) (string, lipgloss.Style) {
	switch status {
	case endpoint.DiagnosisOK:
		return "✓", statusStyleOK
	case endpoint.DiagnosisWarn:
		return "⚠", statusStyleWarn
	case endpoint.DiagnosisFailed:
		return "✗", statusStyleErr
	default:
		return "·", statusStyleDim
	}
}

// statusDuration is how long a layer took, and nothing for a layer whose time
// says nothing: a skipped one, or one that finished inside a millisecond.
func statusDuration(step endpoint.DiagnosisStep) string {
	if step.DurationMS <= 0 {
		return ""
	}
	return (time.Duration(step.DurationMS) * time.Millisecond).String()
}
