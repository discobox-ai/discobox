package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/obot-platform/discobox/internal/apiclient"
)

type eventsOptions struct {
	resources  []string
	afterSeq   int64
	list       bool
	replayOnly bool
}

func (a *App) newEventsCommand() *cobra.Command {
	var opts eventsOptions
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Stream project events",
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, err := a.projectUUID()
			if err != nil {
				return err
			}
			client, err := a.eventClient()
			if err != nil {
				return err
			}
			list := opts.list
			replayOnly := opts.replayOnly
			params := apiclient.ProjectEventsParams{
				Resources:  normalizeResources(opts.resources),
				AfterSeq:   &opts.afterSeq,
				List:       &list,
				ReplayOnly: &replayOnly,
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
	cmd.Flags().StringSliceVar(&opts.resources, "resource", nil, "Resource type to stream; may be repeated or comma-separated")
	cmd.Flags().Int64Var(&opts.afterSeq, "after-seq", -1, "Replay events after this sequence; -1 uses list-watch behavior")
	cmd.Flags().BoolVar(&opts.list, "list", true, "Send a current resource list before live changes")
	cmd.Flags().BoolVar(&opts.replayOnly, "replay-only", false, "Return after replay/list instead of waiting for live events")
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
		fmt.Fprintf(tw, "%s\tid=%s\t%s\n", msg.Event, msg.ID, string(encoded))
	default:
		encoded, _ := json.Marshal(data)
		fmt.Fprintf(tw, "%s\tid=%s\t%s\n", msg.Event, msg.ID, string(encoded))
	}
	return tw.Flush()
}

func normalizeResources(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			result = append(result, part)
		}
	}
	return result
}
