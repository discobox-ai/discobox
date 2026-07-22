package proxy

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	mitmCAName = "mitm-ca"
	mtlsCAName = "mtls-ca"
	serverName = "worker-proxy"
)

// CertificateBundle contains prepared proxy certificate paths and loaded TLS
// material.
type CertificateBundle struct {
	Dir            string
	MITMCAPath     string
	MITMKeyPath    string
	MTLSCAPath     string
	MTLSKeyPath    string
	ServerCertPath string
	ServerKeyPath  string
	MITMCA         tls.Certificate
	MTLSCA         tls.Certificate
	ServerCert     tls.Certificate
	ClientCAPool   *x509.CertPool
}

// PrepareOptions controls certificate preparation.
type PrepareOptions struct {
	Dir            string
	ProxyURL       string
	ServerHosts    []string
	ClientIDs      []string
	NoProxy        string
	Validity       time.Duration
	ClientValidity time.Duration
	RenewBefore    time.Duration
}

// PreparedCertificates is the output of independent certificate preparation.
type PreparedCertificates struct {
	Bundle  *CertificateBundle
	Clients map[string]ClientMaterial
}

// PrepareCertificates creates or reuses proxy certificates without starting a
// proxy listener.
func PrepareCertificates(opts PrepareOptions) (*PreparedCertificates, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("certificate dir is required")
	}
	if opts.ProxyURL == "" {
		opts.ProxyURL = "https://127.0.0.1:17080"
	}
	if len(opts.ServerHosts) == 0 {
		opts.ServerHosts = []string{"127.0.0.1", "localhost"}
	}
	if opts.Validity == 0 {
		opts.Validity = 10 * 365 * 24 * time.Hour
	}
	if opts.ClientValidity == 0 {
		opts.ClientValidity = 365 * 24 * time.Hour
	}
	if opts.RenewBefore == 0 {
		opts.RenewBefore = 30 * 24 * time.Hour
	}
	if opts.NoProxy == "" {
		opts.NoProxy = "127.0.0.1,localhost"
	}

	bundle, err := ensureBundle(opts.Dir, opts.ServerHosts, opts.Validity, opts.RenewBefore)
	if err != nil {
		return nil, err
	}

	clients := make(map[string]ClientMaterial, len(opts.ClientIDs))
	for _, clientID := range opts.ClientIDs {
		material, err := EnsureClientCertificate(bundle, clientID, opts.ProxyURL, opts.NoProxy, opts.ClientValidity, opts.RenewBefore)
		if err != nil {
			return nil, err
		}
		clients[clientID] = material
	}

	return &PreparedCertificates{Bundle: bundle, Clients: clients}, nil
}

// LoadCertificateBundle loads previously prepared certificate material.
func LoadCertificateBundle(dir string) (*CertificateBundle, error) {
	return ensureBundle(dir, []string{"127.0.0.1", "localhost"}, 10*365*24*time.Hour, 30*24*time.Hour)
}

// EnsureClientCertificate creates or reuses a per-client mTLS certificate.
func EnsureClientCertificate(bundle *CertificateBundle, clientID, proxyURL, noProxy string, validity, renewBefore time.Duration) (ClientMaterial, error) {
	if bundle == nil {
		return ClientMaterial{}, fmt.Errorf("certificate bundle is required")
	}
	if clientID == "" {
		return ClientMaterial{}, fmt.Errorf("client id is required")
	}
	clientDir := filepath.Join(bundle.Dir, "clients", filepath.Clean(clientID))
	if err := os.MkdirAll(clientDir, 0o700); err != nil {
		return ClientMaterial{}, fmt.Errorf("create client cert dir: %w", err)
	}
	certPath := filepath.Join(clientDir, "client.crt")
	keyPath := filepath.Join(clientDir, "client.key")
	_, leaf, ok := loadUsableKeyPair(certPath, keyPath, renewBefore)
	if !ok {
		mtlsCA, err := parseLeaf(bundle.MTLSCA)
		if err != nil {
			return ClientMaterial{}, err
		}
		if err := generateSignedCert(certPath, keyPath, signedCertOptions{
			CommonName: clientID,
			Parent:     &bundle.MTLSCA,
			ParentCert: mtlsCA,
			Usage:      []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			Validity:   validity,
		}); err != nil {
			return ClientMaterial{}, fmt.Errorf("generate client certificate: %w", err)
		}
		_, leaf, ok = loadUsableKeyPair(certPath, keyPath, 0)
		if !ok {
			return ClientMaterial{}, fmt.Errorf("generated client certificate is not usable")
		}
	}
	env := map[string]string{
		"HTTP_PROXY":         proxyURL,
		"HTTPS_PROXY":        proxyURL,
		"ALL_PROXY":          proxyURL,
		"NO_PROXY":           noProxy,
		"SSL_CERT_FILE":      bundle.MITMCAPath,
		"REQUESTS_CA_BUNDLE": bundle.MITMCAPath,
	}
	return ClientMaterial{
		ClientID:        clientID,
		ProxyURL:        proxyURL,
		HTTPProxy:       proxyURL,
		HTTPSProxy:      proxyURL,
		AllProxy:        proxyURL,
		NoProxy:         noProxy,
		MITMCAPath:      bundle.MITMCAPath,
		MTLSCAPath:      bundle.MTLSCAPath,
		ClientCertPath:  certPath,
		ClientKeyPath:   keyPath,
		GeneratedAt:     leaf.NotBefore.UTC(),
		ExpiresAt:       leaf.NotAfter.UTC(),
		EnvironmentVars: env,
	}, nil
}

func ensureBundle(dir string, serverHosts []string, validity, renewBefore time.Duration) (*CertificateBundle, error) {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create cert dir: %w", err)
	}

	mitmCert, mitmKey := certPaths(dir, mitmCAName)
	if err := ensureCA(mitmCert, mitmKey, "Discobox Proxy MITM CA", validity, renewBefore); err != nil {
		return nil, err
	}
	mtlsCert, mtlsKey := certPaths(dir, mtlsCAName)
	if err := ensureCA(mtlsCert, mtlsKey, "Discobox Proxy mTLS CA", validity, renewBefore); err != nil {
		return nil, err
	}

	mtlsPair, err := tls.LoadX509KeyPair(mtlsCert, mtlsKey)
	if err != nil {
		return nil, fmt.Errorf("load mtls CA: %w", err)
	}
	mtlsLeaf, err := parseLeaf(mtlsPair)
	if err != nil {
		return nil, err
	}

	serverCert, serverKey := certPaths(dir, serverName)
	_, serverLeaf, serverUsable := loadUsableKeyPair(serverCert, serverKey, renewBefore)
	// A cert that is still in date but no longer covers every requested host is
	// unusable: clients verify the name they dial, so reusing it fails every
	// handshake. This is what makes renaming the proxy's DNS name safe on hosts
	// whose material predates the rename.
	if serverUsable && !certCoversHosts(serverLeaf, serverHosts) {
		serverUsable = false
	}
	if !serverUsable {
		if err := generateSignedCert(serverCert, serverKey, signedCertOptions{
			CommonName: serverName,
			Hosts:      serverHosts,
			Parent:     &mtlsPair,
			ParentCert: mtlsLeaf,
			Usage:      []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			Validity:   validity,
		}); err != nil {
			return nil, fmt.Errorf("generate worker proxy server certificate: %w", err)
		}
	}

	mitmPair, err := tls.LoadX509KeyPair(mitmCert, mitmKey)
	if err != nil {
		return nil, fmt.Errorf("load mitm CA: %w", err)
	}
	serverPair, err := tls.LoadX509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("load server certificate: %w", err)
	}
	caPEM, err := os.ReadFile(mtlsCert)
	if err != nil {
		return nil, fmt.Errorf("read mtls CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse mtls CA pool")
	}

	return &CertificateBundle{
		Dir:            dir,
		MITMCAPath:     mitmCert,
		MITMKeyPath:    mitmKey,
		MTLSCAPath:     mtlsCert,
		MTLSKeyPath:    mtlsKey,
		ServerCertPath: serverCert,
		ServerKeyPath:  serverKey,
		MITMCA:         mitmPair,
		MTLSCA:         mtlsPair,
		ServerCert:     serverPair,
		ClientCAPool:   pool,
	}, nil
}

func certPaths(dir, name string) (string, string) {
	return filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
}

func ensureCA(certPath, keyPath, commonName string, validity, renewBefore time.Duration) error {
	if _, _, ok := loadUsableKeyPair(certPath, keyPath, renewBefore); ok {
		return nil
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate CA serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Discobox"}, CommonName: commonName},
		NotBefore:             time.Now().UTC().Add(-time.Hour),
		NotAfter:              time.Now().UTC().Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create CA certificate: %w", err)
	}
	return writeKeyPair(certPath, keyPath, der, key)
}

type signedCertOptions struct {
	CommonName string
	Hosts      []string
	Parent     *tls.Certificate
	ParentCert *x509.Certificate
	Usage      []x509.ExtKeyUsage
	Validity   time.Duration
}

func generateSignedCert(certPath, keyPath string, opts signedCertOptions) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Discobox"}, CommonName: opts.CommonName},
		NotBefore:             time.Now().UTC().Add(-time.Hour),
		NotAfter:              time.Now().UTC().Add(opts.Validity),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           opts.Usage,
		BasicConstraintsValid: true,
	}
	for _, host := range opts.Hosts {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, opts.ParentCert, &key.PublicKey, opts.Parent.PrivateKey)
	if err != nil {
		return fmt.Errorf("create signed certificate: %w", err)
	}
	return writeKeyPair(certPath, keyPath, der, key)
}

func writeKeyPair(certPath, keyPath string, certDER []byte, key *rsa.PrivateKey) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return err
	}
	certFile, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		_ = certFile.Close()
		return err
	}
	if err := certFile.Close(); err != nil {
		return err
	}
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		_ = keyFile.Close()
		return err
	}
	return keyFile.Close()
}

func parseLeaf(cert tls.Certificate) (*x509.Certificate, error) {
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("certificate has no leaf")
	}
	return x509.ParseCertificate(cert.Certificate[0])
}

func loadUsableKeyPair(certPath, keyPath string, renewBefore time.Duration) (tls.Certificate, *x509.Certificate, bool) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, nil, false
	}
	leaf, err := parseLeaf(pair)
	if err != nil {
		return tls.Certificate{}, nil, false
	}
	if renewBefore < 0 {
		renewBefore = 0
	}
	now := time.Now().UTC()
	if now.Before(leaf.NotBefore) || !leaf.NotAfter.After(now.Add(renewBefore)) {
		return tls.Certificate{}, nil, false
	}
	return pair, leaf, true
}

// certCoversHosts reports whether leaf is valid for every requested host, so a
// host list that has grown or been renamed forces reissue instead of serving a
// certificate clients will reject.
func certCoversHosts(leaf *x509.Certificate, hosts []string) bool {
	if leaf == nil {
		return false
	}
	for _, host := range hosts {
		if leaf.VerifyHostname(host) != nil {
			return false
		}
	}
	return true
}

// SignHost signs a short-lived MITM certificate for host using bundle's MITM CA.
func (b *CertificateBundle) SignHost(host string) (tls.Certificate, error) {
	caCert, err := parseLeaf(b.MITMCA)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Discobox"}, CommonName: host},
		NotBefore:             time.Now().UTC().Add(-time.Hour),
		NotAfter:              time.Now().UTC().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, caCert, &key.PublicKey, b.MITMCA.PrivateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
