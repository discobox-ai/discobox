package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/obot-platform/discobox/apiclient"
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
			client, err := a.eventClient()
			if err != nil {
				return err
			}
			history := opts.history
			listOnly := opts.listOnly
			params := apiclient.ProjectEventsParams{
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

func (a *App) writeEvent(cmd *cobra.Command, msg *apiclient.ProjectEventMessage) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), msg)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	switch data := msg.Data.(type) {
	case *apiclient.ResourceChangedEvent:
		fmt.Fprintf(tw, "%s\tseq=%d\t%s\t%s/%s\t%s\n", msg.Event, data.Seq, data.Action, data.ResourceType, data.ResourceID, data.CreatedAt.Local().Format("2006-01-02T15:04:05Z07:00"))
	case *apiclient.ResourceListedEvent:
		fmt.Fprintf(tw, "%s\tseq=%d\t%s\t%s/%s\t%s\n", msg.Event, data.Seq, data.Action, data.ResourceType, data.ResourceID, data.CreatedAt.Local().Format("2006-01-02T15:04:05Z07:00"))
	case *apiclient.ResourceListStartEvent:
		fmt.Fprintf(tw, "%s\tseq=%d\tresources=%s\t%s\n", msg.Event, data.Seq, strings.Join(data.Resources, ","), data.StartedAt.Local().Format("2006-01-02T15:04:05Z07:00"))
	case *apiclient.ResourceListFinishEvent:
		fmt.Fprintf(tw, "%s\tseq=%d\tresources=%s\t%s\n", msg.Event, data.Seq, strings.Join(data.Resources, ","), data.FinishedAt.Local().Format("2006-01-02T15:04:05Z07:00"))
	case *apiclient.UnknownProjectEvent:
		encoded, _ := json.Marshal(data.Data)
		fmt.Fprintf(tw, "%s\t%s\n", msg.Event, string(encoded))
	default:
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(tw, "%s\t%s\n", msg.Event, string(encoded))
	}
	return tw.Flush()
}
