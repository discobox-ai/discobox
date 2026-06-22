package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
)

type eventsOptions struct {
	history   bool
	listOnly  bool
	sandboxID string
}

func (a *App) newEventsCommand() *cobra.Command {
	var opts eventsOptions
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream project events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			history := opts.history
			listOnly := opts.listOnly
			params := apiclientgen.ProjectEventsParams{
				History:   &history,
				ListOnly:  &listOnly,
				SandboxID: opts.sandboxID,
			}
			stream, err := client.SubscribeProjectEvents(cmd.Context(), projectID, params)
			if err != nil {
				return err
			}
			defer stream.Close()
			for {
				msg, err := stream.Read()
				if err != nil {
					if errors.Is(err, io.EOF) {
						return nil
					}
					return err
				}
				if err := a.writeEvent(cmd, msg); err != nil {
					return err
				}
			}
		},
	}
	cmd.Flags().BoolVar(&opts.history, "history", true, "Send current resource data before live changes")
	cmd.Flags().BoolVar(&opts.listOnly, "list-only", false, "Return after list instead of waiting for live events")
	cmd.Flags().StringVar(&opts.sandboxID, "sandbox", "", "Sandbox ID to stream; defaults to all sandboxes")
	return cmd
}

func (a *App) writeEvent(cmd *cobra.Command, msg *apiclientgen.ProjectEventMessage) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), msg)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	switch data := msg.Data.(type) {
	case *apiclientgen.ResourceChangedEvent:
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s/%s\t%s\n", msg.Event, eventIDOrSeq(data.ID, data.Seq), data.Action, data.ResourceType, data.ResourceID, data.CreatedAt.Local().Format("2006-01-02T15:04:05Z07:00"))
	case *apiclientgen.ResourceListedEvent:
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s/%s\t%s\n", msg.Event, eventIDOrSeq(data.ID, data.Seq), data.Action, data.ResourceType, data.ResourceID, data.CreatedAt.Local().Format("2006-01-02T15:04:05Z07:00"))
	case *apiclientgen.ResourceListStartEvent:
		fmt.Fprintf(tw, "%s\tseq=%d\tresources=%s\t%s\n", msg.Event, data.Seq, strings.Join(data.Resources, ","), data.StartedAt.Local().Format("2006-01-02T15:04:05Z07:00"))
	case *apiclientgen.ResourceListFinishEvent:
		fmt.Fprintf(tw, "%s\tseq=%d\tresources=%s\t%s\n", msg.Event, data.Seq, strings.Join(data.Resources, ","), data.FinishedAt.Local().Format("2006-01-02T15:04:05Z07:00"))
	case *apiclientgen.UnknownProjectEvent:
		encoded, err := json.Marshal(data.Data)
		if err != nil {
			return fmt.Errorf("encode event data: %w", err)
		}
		fmt.Fprintf(tw, "%s\t%s\n", msg.Event, string(encoded))
	default:
		encoded, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("encode event data: %w", err)
		}
		fmt.Fprintf(tw, "%s\t%s\n", msg.Event, string(encoded))
	}
	return tw.Flush()
}

func eventIDOrSeq(id string, seq int64) string {
	if strings.TrimSpace(id) != "" {
		return shortID(id)
	}
	return fmt.Sprintf("seq=%d", seq)
}
