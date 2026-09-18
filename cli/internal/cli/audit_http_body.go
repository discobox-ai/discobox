package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/auditid"
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// The recordings `audit http --body` reads, by --part, and the artifact each
// names on the server's route.
const (
	httpAuditPartResponse = "response"
	httpAuditPartRequest  = "request"
	httpAuditPartStream   = "stream"
)

// httpAuditFormatHeader names how a recording was spooled: raw bytes for a
// body, framed chunks for an upgraded stream.
const httpAuditFormatHeader = "X-Discobox-Audit-Format"

// writeHTTPAuditArtifact prints one recording from the pool trail. It is read
// from a hand-wired route rather than the generated client, because a body is
// an unbounded stream the generated client would buffer.
func (a *App) writeHTTPAuditArtifact(cmd *cobra.Command, projectID, poolID, sandboxID string, id auditid.ExchangeID, part string) error {
	artifact := map[string]string{
		httpAuditPartResponse: "response-body",
		httpAuditPartRequest:  "request-body",
		httpAuditPartStream:   "stream",
	}[part]
	if artifact == "" {
		return fmt.Errorf("--part %q: want response, request or stream", part)
	}
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/projects/" + url.PathEscape(projectID) + "/pools/" + url.PathEscape(poolID) +
		"/audit/http/" + id.String() + "/" + artifact
	if sandboxID != "" {
		u.RawQuery = url.Values{"sandboxId": []string{sandboxID}}.Encode()
	}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("read recorded %s: %w", part, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		if message := attachErrorMessage(body); message != "" {
			return fmt.Errorf("read recorded %s: %s", part, message)
		}
		return fmt.Errorf("read recorded %s: %s", part, resp.Status)
	}
	if format := resp.Header.Get(httpAuditFormatHeader); format != "" && format != "raw" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "recorded as %s\n", terminalSafe(format))
	}
	out := cmd.OutOrStdout()
	if isTerminalStream(out) {
		// What a service answered a discobox is display data like everything
		// else it recorded (ADR 0130 §6), and a body is the likeliest place
		// for bytes a terminal would act on.
		return copyTerminalSafe(out, resp.Body)
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// copyTerminalSafe copies src to dst as terminalSafeMultiline would print it,
// without holding all of it: a rune split across two reads is carried to the
// next one rather than escaped as two invalid bytes.
func copyTerminalSafe(dst io.Writer, src io.Reader) error {
	buf := make([]byte, 32*1024)
	carry := 0
	for {
		n, err := src.Read(buf[carry:])
		n += carry
		end := n
		if err == nil {
			// Hold back an incomplete rune at the end for the next read.
			for back := 1; back < utf8.UTFMax && back <= end; back++ {
				if utf8.RuneStart(buf[end-back]) {
					if !utf8.FullRune(buf[end-back : end]) {
						end -= back
					}
					break
				}
			}
		}
		if _, werr := io.WriteString(dst, terminalSafeMultiline(string(buf[:end]))); werr != nil {
			return werr
		}
		carry = copy(buf, buf[end:n])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// auditRecordPool is the pool an audit record belongs to: the one named, or the
// one the discobox runs on. A record ID is only unique within its pool, and a
// discobox runs on one for its life, so naming the discobox is enough — except
// for a deleted one, whose pool nothing records any more.
func (a *App) auditRecordPool(ctx context.Context, client *apiclientgen.Client, projectID, poolID, sandboxID string) (string, error) {
	if poolID != "" {
		return poolID, nil
	}
	if sandboxID == "" {
		return "", errors.New("--pool or --discobox-id is required: a record's ID is only unique on the pool that recorded it")
	}
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return "", err
	}
	box, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return "", fmt.Errorf("resolve the pool of discobox %s: %w; pass --pool", terminalSafe(sandboxID), err)
	}
	pool := box.PoolId.Or("")
	if pool == "" {
		return "", fmt.Errorf("discobox %s is not on a pool; pass --pool", terminalSafe(sandboxID))
	}
	return pool, nil
}
