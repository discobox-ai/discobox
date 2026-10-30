package bridge

import (
	"encoding/json"
	"fmt"
	"os"
)

// File is a bridge config as the sandbox's runtime-config intake renders it
// into the proxy material directory, and as every reader of one decodes it:
// the egress, nested-Docker and BuildKit forwarders, the credentials relay and
// the DNS stub (ADR 26-10-08-127 §4). It is one type so a field the intake
// writes reaches every reader: a reader with a copy of its own that missed one
// decodes the file without error and dials the wrong thing.
type File struct {
	// ListenAddress is where the sandbox's own forwarder listens; the
	// nested-Docker bridge names none, and discovers it.
	ListenAddress string `json:"listenAddress"`
	// UpstreamURL is the pool service the forwarder reaches, a wire URL.
	UpstreamURL string `json:"workerProxyUrl"`
	// ServerName is the name the pool's certificate is verified as; see
	// DialConfig.ServerName.
	ServerName string `json:"serverName,omitempty"`
	// Address is discobox's own answer for that name; see DialConfig.Address.
	Address string `json:"address,omitempty"`
	// CredentialsURL is the pool's agent-credentials endpoint (ADR 0031),
	// which only the egress bridge names.
	CredentialsURL string `json:"credentialsUrl,omitempty"`
	// DNSServer and DNSListenAddress are the pool's DNS server and where the
	// sandbox's stub listens (ADR 0148), which only the egress bridge names.
	DNSServer        string `json:"dnsServer,omitempty"`
	DNSListenAddress string `json:"dnsListenAddress,omitempty"`
	MTLSCAPath       string `json:"mtlsCaPath"`
	ClientCertPath   string `json:"clientCertPath"`
	ClientKeyPath    string `json:"clientKeyPath"`
}

// LoadFile reads the bridge config at path.
func LoadFile(path string) (File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return file, nil
}

// Dial is how a reader of f reaches the pool service at url: f's name, its
// answer for the name, and the sandbox's material.
func (f File) Dial(url string) DialConfig {
	return DialConfig{
		URL:            url,
		ServerName:     f.ServerName,
		Address:        f.Address,
		MTLSCAPath:     f.MTLSCAPath,
		ClientCertPath: f.ClientCertPath,
		ClientKeyPath:  f.ClientKeyPath,
	}
}
