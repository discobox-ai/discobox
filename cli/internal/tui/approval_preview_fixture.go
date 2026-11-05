//go:build prototype

package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/requestrole"
	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/credentialpolicy"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// loadApprovalPreviewRequest reads the same JSON input as discobox-access.
// Policy paths are relative to the supplied fixture root (the manager checkout).
func loadApprovalPreviewRequest(path, root string) (*CredentialRequest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var body agentcreds.RequestBody
	if err = json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	req := &CredentialRequest{ID: "sreq_manager_sample", SandboxID: "sbx_manager", Name: body.Name, EnvVar: body.EnvVar, Justification: body.Justification, AskedTTL: time.Duration(body.GrantTTLSeconds) * time.Second, Created: time.Now().Add(-2 * time.Minute)}
	resolve := func(policies []agentcreds.Policy, routes []string) (*RequestPart, error) {
		part := &RequestPart{}
		var grouped []requestrole.Policy
		for _, ref := range policies {
			doc := ref.Document
			if len(doc) == 0 {
				var err error
				doc, err = os.ReadFile(filepath.Join(root, ref.File))
				if err != nil {
					return nil, err
				}
			}
			resolved, err := credentialpolicy.ResolveDocument(doc, ref.Args)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ref.File, err)
			}
			grouped = append(grouped, requestrole.Policy{File: ref.File, Name: strings.TrimSuffix(filepath.Base(ref.File), ".json"), Description: resolved.Description, Args: ref.Args, Digest: resolved.Digest, UnusedArgs: resolved.UnusedArgs, Routes: resolved.Routes})
			part.Routes = append(part.Routes, resolved.Routes...)
		}
		part.Roles = requestrole.Group(grouped)
		for _, raw := range routes {
			route, err := secretroute.Parse(raw)
			if err != nil {
				return nil, err
			}
			part.Routes = append(part.Routes, route)
		}
		// Match the server's NormalizeRoutes: canonicalize and deduplicate,
		// retaining first occurrence order.
		seen := map[string]bool{}
		var normalized []secretroute.Route
		for _, route := range part.Routes {
			next, err := secretroute.Normalize(route)
			if err != nil {
				return nil, err
			}
			key := secretroute.Format(next)
			if !seen[key] {
				normalized = append(normalized, next)
				seen[key] = true
			}
		}
		part.Routes = normalized
		return part, nil
	}
	if body.Uses() {
		req.Use, err = resolve(body.Policies, body.Routes)
		if err != nil {
			return nil, err
		}
		req.Use.Add = body.Add
		for _, host := range body.Hosts {
			req.Use.Routes = append(req.Use.Routes, secretroute.Route{Hosts: []string{host}})
		}
	}
	if body.Delegate != nil {
		req.Delegate, err = resolve(body.Delegate.Policies, body.Delegate.Routes)
		if err != nil {
			return nil, err
		}
	}
	return req, nil
}
