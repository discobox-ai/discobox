// Executed against the real image and core provisioning output, in the
// application's workspace so MaterializeTrustStore is the production code.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/runcca"
)

func main() {
	root := flag.String("root", "/", "image root filesystem")
	bundle := flag.String("bundle", "/opt/discobox/ca-certificates/ca-certificates.crt", "public bundle relative to image root")
	runtime := flag.Bool("runtime", false, "exercise production materializer and TLS")
	flag.Parse()
	if err := check(*root, *bundle, *runtime); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func certificates(body []byte) (map[string]*x509.Certificate, error) {
	result := map[string]*x509.Certificate{}
	for len(bytes.TrimSpace(body)) > 0 {
		block, rest := pem.Decode(body)
		if block == nil {
			return nil, fmt.Errorf("invalid PEM certificate data")
		}
		body = rest
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block %s", block.Type)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		result[string(cert.Raw)] = cert
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty certificate bundle")
	}
	return result, nil
}
func readCertificates(path string) (map[string]*x509.Certificate, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return certificates(body)
}
func publicBundle(root, bundle string) (map[string]*x509.Certificate, error) {
	expected := map[string]*x509.Certificate{}
	config, err := os.ReadFile(filepath.Join(root, "etc/ca-certificates.conf"))
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(config), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		certs, err := readCertificates(filepath.Join(root, "usr/share/ca-certificates", line))
		if err != nil {
			return nil, err
		}
		for der, c := range certs {
			expected[der] = c
		}
	}
	actual, err := readCertificates(filepath.Join(root, bundle))
	if err != nil {
		return nil, err
	}
	if len(expected) == 0 || len(actual) != len(expected) {
		return nil, fmt.Errorf("public store has %d certificates; distro configuration requires %d", len(actual), len(expected))
	}
	for der := range expected {
		if actual[der] == nil {
			return nil, fmt.Errorf("public store omits enabled distro certificate")
		}
	}
	return actual, nil
}
func tlsFixture() (server *httptest.Server, anchor []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable runtime CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	anchor = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "verified HTTPS\n") }))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: key}}}
	server.StartTLS()
	return server, anchor, nil
}
func request(bundle, url string, accept bool) error {
	body, err := os.ReadFile(bundle)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return fmt.Errorf("invalid client roots")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	response, err := client.Get(url)
	if !accept {
		if response != nil {
			response.Body.Close()
		}
		var unknown x509.UnknownAuthorityError
		if !errors.As(err, &unknown) {
			return fmt.Errorf("expected unknown-CA rejection, got %v", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err = io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode != 200 || string(body) != "verified HTTPS\n" {
		return fmt.Errorf("unexpected HTTPS response")
	}
	return nil
}
func check(root, bundle string, runtime bool) error {
	public, err := publicBundle(root, bundle)
	if err != nil {
		return err
	}
	report := map[string]any{"publicCertificates": len(public), "exactDistroRoots": true, "builderRootsAbsent": true}
	if runtime {
		server, anchor, err := tlsFixture()
		if err != nil {
			return err
		}
		defer server.Close()
		other, _, err := tlsFixture()
		if err != nil {
			return err
		}
		defer other.Close()
		publicPath := filepath.Join(root, bundle)
		if err = request(publicPath, server.URL, false); err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "discobox-trust-check-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		// Prove the saved-artifact guard catches an extra build-time CA, even
		// when its subject does not contain the builder's product name.
		publicBody, err := os.ReadFile(publicPath)
		if err != nil {
			return err
		}
		contaminated := filepath.Join(dir, "contaminated.pem")
		if err = os.WriteFile(contaminated, append(publicBody, anchor...), 0600); err != nil {
			return err
		}
		if _, err = publicBundle(root, contaminated); err == nil {
			return fmt.Errorf("saved-artifact check accepted an unexpected CA")
		}
		anchors := filepath.Join(dir, "anchors")
		if err = os.MkdirAll(anchors, 0700); err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(anchors, "runtime.crt"), anchor, 0600); err != nil {
			return err
		}
		store := filepath.Join(dir, "store")
		if err = runcca.MaterializeTrustStore(store, filepath.Dir(publicPath), []string{anchors}); err != nil {
			return err
		}
		runtimePath := filepath.Join(store, "ca-certificates.crt")
		before, err := os.ReadFile(runtimePath)
		if err != nil {
			return err
		}
		actual, err := certificates(before)
		if err != nil {
			return err
		}
		if len(actual) != len(public)+1 {
			return fmt.Errorf("runtime store contains unexpected roots")
		}
		for der := range public {
			if actual[der] == nil {
				return fmt.Errorf("runtime store lost public root")
			}
		}
		if err = request(runtimePath, server.URL, true); err != nil {
			return err
		}
		if err = request(runtimePath, other.URL, false); err != nil {
			return err
		}
		if err = runcca.MaterializeTrustStore(store, filepath.Dir(publicPath), []string{anchors}); err != nil {
			return err
		}
		after, err := os.ReadFile(runtimePath)
		if err != nil {
			return err
		}
		if !bytes.Equal(before, after) {
			return fmt.Errorf("runtime CA addition is not idempotent")
		}
		if _, err = publicBundle(root, bundle); err != nil {
			return err
		}
		report["runtimeHTTPS"] = true
		report["unknownCARejected"] = true
		report["publicBundleUnchanged"] = true
		report["runtimeAdditionIdempotent"] = true
		report["contaminatedBundleRejected"] = true
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
