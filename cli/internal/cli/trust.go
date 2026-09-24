package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
)

// Host trust (ADR 0149): the hosts a discobox trusts by a pin, and the asks
// to trust one that are waiting on a person. The window answers the same asks
// from its inbox; these are the same answers from a shell.

func (a *App) newTrustCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "trust", Aliases: []string{"trusts"}, Short: "Manage the hosts discoboxes trust by a pinned certificate"}
	cmd.AddCommand(a.newTrustListCommand())
	cmd.AddCommand(a.newTrustRemoveCommand())
	cmd.AddCommand(a.newTrustRequestCommand())
	return cmd
}

func (a *App) newTrustRequestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "request", Aliases: []string{"requests"}, Short: "Answer agents' asks to trust a host"}
	cmd.AddCommand(a.newTrustRequestListCommand())
	cmd.AddCommand(a.newTrustRequestGetCommand())
	cmd.AddCommand(a.newTrustRequestApproveCommand())
	cmd.AddCommand(a.newTrustRequestDenyCommand())
	return cmd
}

func (a *App) newTrustListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls DISCOBOX", Aliases: []string{"list"}, Short: "List the hosts a discobox trusts", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		sandboxID, err := a.resolveSandboxID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		res, err := client.ListSandboxHostTrusts(cmd.Context(), apiclientgen.ListSandboxHostTrustsParams{ProjectId: projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListHostTrustsBody](res)
		if err != nil {
			return err
		}
		return a.writeHostTrusts(cmd, body.GetHostTrusts())
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newTrustRemoveCommand() *cobra.Command {
	return &cobra.Command{Use: "rm DISCOBOX TRUST_ID...", Aliases: []string{"revoke"}, Short: "Revoke a discobox's trust of a host", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		sandboxID, err := a.resolveSandboxID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		for _, trustID := range args[1:] {
			res, err := client.DeleteSandboxHostTrust(cmd.Context(), apiclientgen.DeleteSandboxHostTrustParams{ProjectId: projectID, SandboxId: sandboxID, TrustId: trustID})
			if err != nil {
				return err
			}
			if err := expectNoContent[apiclientgen.DeleteSandboxHostTrustNoContent](res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s revoked\n", trustID)
		}
		return nil
	}}
}

func (a *App) newTrustRequestListCommand() *cobra.Command {
	var status string
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List trust requests", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		params := apiclientgen.ListTrustRequestsParams{ProjectId: projectID}
		if s := strings.TrimSpace(status); s != "" {
			params.Status = apiclientgen.NewOptListTrustRequestsStatus(apiclientgen.ListTrustRequestsStatus(s))
		}
		res, err := client.ListTrustRequests(cmd.Context(), params)
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListTrustRequestsBody](res)
		if err != nil {
			return err
		}
		return a.writeTrustRequests(cmd, body.GetTrustRequests())
	}}
	cmd.Flags().StringVar(&status, "status", "", "Filter by status: pending, approved, or denied")
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newTrustRequestGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get REQUEST_ID", Short: "Show a trust request and the chain the pool was shown", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		requestID, err := a.resolveTrustRequestID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		res, err := client.GetTrustRequest(cmd.Context(), apiclientgen.GetTrustRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		request, err := expectResponse[apimodel.HostTrustRequest](res)
		if err != nil {
			return err
		}
		return a.writeTrustRequest(cmd, request)
	}}
}

func (a *App) newTrustRequestApproveCommand() *cobra.Command {
	var pin, ttl string
	var uses []string
	cmd := &cobra.Command{Use: "approve REQUEST_ID", Short: "Trust the host for the asking discobox", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		requestID, err := a.resolveTrustRequestID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		body := &apimodel.ApproveTrustRequestBody{}
		if strings.TrimSpace(pin) != "" {
			parsed, err := parseTrustPin(pin)
			if err != nil {
				return err
			}
			body.SetPin(apiclientgen.NewOptTrustPin(parsed))
		}
		// Sent only when given: the server then takes what the agent asked
		// for, or an hour, which is what the window opens on too.
		if strings.TrimSpace(ttl) != "" {
			d, err := lifetime.Parse(ttl)
			if err != nil {
				return err
			}
			if d <= 0 {
				return fmt.Errorf("a trust always lapses; give --ttl as a lifetime such as 4h or 7d")
			}
			body.SetGrantTTLSeconds(apiclientgen.NewOptInt64(lifetime.Seconds(d)))
		}
		if len(uses) > 0 {
			approved := make([]apimodel.SecretUse, 0, len(uses))
			for _, use := range uses {
				approved = append(approved, apimodel.SecretUse{Description: use})
			}
			body.SetUses(apiclientgen.NewOptNilSecretUseArray(approved))
		}
		res, err := client.ApproveTrustRequest(cmd.Context(), body, apiclientgen.ApproveTrustRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		request, err := expectResponse[apimodel.HostTrustRequest](res)
		if err != nil {
			return err
		}
		return a.writeTrustRequest(cmd, request)
	}}
	cmd.Flags().StringVar(&pin, "pin", "", "Certificate to pin, as ca:SHA256 or leaf-spki:SHA256 from `discobox trust request get`; omit for the server's default (a supplied CA, else the chain's CA, else the host's key)")
	cmd.Flags().StringVar(&ttl, "ttl", "", "How long the trust lasts, up to 30d (defaults to what the agent asked for, or 1h)")
	cmd.Flags().StringArrayVar(&uses, "use", nil, "Replace the agent's declared uses with these (repeatable); omit to approve them as asked")
	return cmd
}

func (a *App) newTrustRequestDenyCommand() *cobra.Command {
	return &cobra.Command{Use: "deny REQUEST_ID", Short: "Deny a trust request", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		requestID, err := a.resolveTrustRequestID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		res, err := client.DenyTrustRequest(cmd.Context(), apiclientgen.DenyTrustRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		if err := expectNoContent[apiclientgen.DenyTrustRequestNoContent](res); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s denied\n", requestID)
		return nil
	}}
}

// parseTrustPin reads a pin as `discobox trust request get` prints it.
func parseTrustPin(value string) (apimodel.TrustPin, error) {
	kind, sha, ok := strings.Cut(strings.TrimSpace(value), ":")
	sha = strings.ToLower(strings.TrimSpace(sha))
	if !ok || sha == "" {
		return apimodel.TrustPin{}, fmt.Errorf("--pin is ca:SHA256 or leaf-spki:SHA256")
	}
	switch apiclientgen.TrustPinKind(kind) {
	case apiclientgen.TrustPinKindCa, apiclientgen.TrustPinKindLeafSpki:
		return apimodel.TrustPin{Kind: apiclientgen.TrustPinKind(kind), SHA256: sha}, nil
	default:
		return apimodel.TrustPin{}, fmt.Errorf("--pin kind is ca or leaf-spki, not %q", kind)
	}
}

func (a *App) resolveTrustRequestID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "trust request ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListTrustRequests(ctx, apiclientgen.ListTrustRequestsParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListTrustRequestsBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetTrustRequests()))
	for _, request := range body.GetTrustRequests() {
		ids = append(ids, request.ID)
	}
	return resolveShortID(id, "trust request ID", ids)
}

func (a *App) writeTrustRequests(cmd *cobra.Command, requests []apimodel.HostTrustRequest) error {
	requests = sortedByRecency(requests, func(request apimodel.HostTrustRequest) time.Time {
		return recencyTime(request.UpdatedAt, request.CreatedAt)
	})
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), requests, func(request apimodel.HostTrustRequest) string { return request.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"trustRequests": requests})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tHOST\tSTATUS\tDISCOBOX\tTRUST\tUPDATED")
	for _, request := range requests {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			request.ID, request.Host, request.Status, request.SandboxId, request.TrustId.Or(""), formatTime(request.UpdatedAt))
	}
	return tw.Flush()
}

// writeTrustRequest shows everything a pin is decided on: the ask, and each
// certificate the pool was shown with the hashes --pin takes.
func (a *App) writeTrustRequest(cmd *cobra.Command, request *apimodel.HostTrustRequest) error {
	if request == nil {
		return nil
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), request)
	}
	out := cmd.OutOrStdout()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE")
	fmt.Fprintf(tw, "ID\t%s\n", request.ID)
	fmt.Fprintf(tw, "HOST\t%s\n", request.Host)
	fmt.Fprintf(tw, "DISCOBOX\t%s\n", request.SandboxId)
	fmt.Fprintf(tw, "STATUS\t%s\n", request.Status)
	if trustID := request.TrustId.Or(""); trustID != "" {
		fmt.Fprintf(tw, "TRUST\t%s\n", trustID)
	}
	if justification := request.Justification.Or(""); justification != "" {
		fmt.Fprintf(tw, "WHY\t%s\n", justification)
	}
	for _, use := range request.Uses.Or(nil) {
		fmt.Fprintf(tw, "USE\t%s\n", use.Description)
	}
	if asked := lifetime.FromRequest(request.GrantTTLSeconds.Or(0)); asked > 0 {
		fmt.Fprintf(tw, "WANTED FOR\t%s\n", lifetime.Label(asked))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Chain the pool was shown, leaf first:")
	for i, cert := range request.ObservedChain.Or(nil) {
		writeObservedCertificate(out, fmt.Sprintf("[%d]", i), cert)
	}
	if supplied, ok := request.SuppliedCA.Get(); ok {
		fmt.Fprintln(out, "CA the agent supplied, which the chain verifies against:")
		writeObservedCertificate(out, "[ca]", supplied)
	}
	return nil
}

func writeObservedCertificate(out interface{ Write([]byte) (int, error) }, label string, cert apimodel.ObservedCertificate) {
	fmt.Fprintf(out, "  %s %s\n", label, cert.Subject)
	fmt.Fprintf(out, "      issuer   %s\n", cert.Issuer)
	names := append(cert.DnsNames.Or(nil), cert.Ips.Or(nil)...)
	if len(names) > 0 {
		fmt.Fprintf(out, "      names    %s\n", strings.Join(names, ", "))
	}
	fmt.Fprintf(out, "      valid    %s to %s\n", cert.NotBefore.Format(time.DateOnly), cert.NotAfter.Format(time.DateOnly))
	if cert.IsCA.Or(false) {
		fmt.Fprintf(out, "      pin      ca:%s\n", cert.SHA256)
	}
	if label == "[0]" {
		fmt.Fprintf(out, "      pin      leaf-spki:%s\n", cert.SpkiSha256)
	}
}

func (a *App) writeHostTrusts(cmd *cobra.Command, trusts []apimodel.HostTrust) error {
	if a.quiet {
		return writeResourceIDs(cmd.OutOrStdout(), trusts, func(trust apimodel.HostTrust) string { return trust.ID })
	}
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"hostTrusts": trusts})
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tHOST\tPIN\tEXPIRES\tUSES")
	for _, trust := range trusts {
		descriptions := make([]string, 0)
		for _, use := range trust.Uses.Or(nil) {
			descriptions = append(descriptions, use.Description)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s:%s\t%s\t%s\n",
			trust.ID, trust.Host, trust.Pin.Kind, shortHash(trust.Pin.SHA256), formatTime(trust.ExpiresAt), strings.Join(descriptions, "; "))
	}
	return tw.Flush()
}

func shortHash(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:16]
}
