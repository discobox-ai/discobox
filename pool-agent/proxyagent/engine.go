package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/proxy"
)

// EngineClientID is the pool proxy client a machine pool's own engine calls
// its driver's service as, where the pool's provisioning key file holds a
// sentinel for the driver's key rather than a key (ADR 26-11-11-080 §9). Every
// sandbox ID starts sbx_ (the control plane's id.PrefixSandbox), so no
// sandbox, nor any build step, which presents its sandbox's certificate, can
// be this client. It names its certificate's directory, so it is a name every
// OS takes (no colon: Windows refuses one). Its client certificate is root's
// alone, in the pool's certificate directory.
//
// Its sentinel is swapped only where its grant, written from where the driver
// declares it sends its key, allows it; resolving it asks the control plane
// for the pool's provider's key (resolve-driver-key), never a sandbox's
// secret; and the answer to every request it swaps into is never recorded
// (proxy.SecretClient.Exchanges), since a driver can trade its key there for
// a credential of its own.
const EngineClientID = "pool-engine"

// EnsureEngineMaterial issues (or reuses, or renews) the engine's client
// certificate, and returns it with the pool's CAs: the mTLS CA the proxy's
// server certificate is verified by, and the MITM CA the engine trusts the
// driver's service through the proxy by.
func EnsureEngineMaterial(root layout.Root, projectID, poolID string) (proxy.ClientMaterial, error) {
	if err := errors.Join(validateIDSegment("project", projectID), validateIDSegment("pool", poolID)); err != nil {
		return proxy.ClientMaterial{}, err
	}
	bundle, err := PrepareBundle(root, projectID, poolID)
	if err != nil {
		return proxy.ClientMaterial{}, err
	}
	material, err := proxy.EnsureClientCertificate(bundle, EngineClientID, PoolProxyURL, "", clientCertValidity, clientCertRenewBefore)
	if err != nil {
		return proxy.ClientMaterial{}, fmt.Errorf("ensure the pool engine's proxy certificate: %w", err)
	}
	return material, nil
}

type resolveDriverKeyBody struct {
	Sentinel string `json:"sentinel"`
	Host     string `json:"host"`
}

type resolveDriverKeyResponse struct {
	Value     string     `json:"value"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// resolveDriverKey asks the control plane for the pool's provider's key, as
// it goes where the driver sends it to host, for the engine's sentinel (ADR
// 26-11-11-080 §9). It is answered as a sandbox's secret is: an answer about
// the control plane or the pool's own assertion is an error, which the proxy
// asks again and then refuses the request on, rather than sending the
// sentinel; any other refusal is a denial.
func (r *secretResolver) resolveDriverKey(ctx context.Context, req proxy.SecretResolveRequest) (proxy.SecretResolveResult, error) {
	rc, err := readResolveContext(r.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		return proxy.SecretResolveResult{}, errors.New("resolve the driver's key: no control plane credential yet")
	}
	payload, err := json.Marshal(resolveDriverKeyBody{Sentinel: req.Sentinel, Host: req.Host})
	if err != nil {
		return proxy.SecretResolveResult{}, err
	}
	url := fmt.Sprintf("%s/pool-agent/%s/resolve-driver-key", rc.ControlPlaneURL, rc.PoolID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return proxy.SecretResolveResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+rc.Token)
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return proxy.SecretResolveResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusInternalServerError || resolveStatusTransient(resp.StatusCode) {
		return proxy.SecretResolveResult{}, fmt.Errorf("resolve the driver's key: control plane returned %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	var out resolveDriverKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return proxy.SecretResolveResult{}, err
	}
	if out.Value == "" {
		return proxy.SecretResolveResult{}, proxy.ErrSecretResolveDenied
	}
	result := proxy.SecretResolveResult{Value: out.Value}
	if out.ExpiresAt != nil {
		result.ExpiresAt = *out.ExpiresAt
	}
	return result, nil
}
