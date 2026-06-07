package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/disco2/internal/apiclient/gen"
)

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func (a *App) writeSandbox(cmd *cobra.Command, sandbox *apiclientgen.Sandbox) error {
	if sandbox == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), sandbox)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPHASE\tDESIRED\tGENERATION\tUPDATED")
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
		sandbox.ID,
		sandbox.Name,
		sandbox.Phase,
		sandbox.DesiredState,
		sandbox.Generation,
		formatTime(sandbox.UpdatedAt),
	)
	return tw.Flush()
}

func (a *App) writeSandboxes(cmd *cobra.Command, sandboxes []apiclientgen.Sandbox) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"sandboxes": sandboxes})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tPHASE\tDESIRED\tGENERATION\tUPDATED")
	for _, sandbox := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			sandbox.ID,
			sandbox.Name,
			sandbox.Phase,
			sandbox.DesiredState,
			sandbox.Generation,
			formatTime(sandbox.UpdatedAt),
		)
	}
	return tw.Flush()
}

func parseUUIDArg(value, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	return id, nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Local().Format(time.RFC3339)
}

func optString(value string) apiclientgen.OptString {
	if strings.TrimSpace(value) == "" {
		return apiclientgen.OptString{}
	}
	return apiclientgen.NewOptString(value)
}
