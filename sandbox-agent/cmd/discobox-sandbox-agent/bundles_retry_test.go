//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/sandbox-agent/bundlestore"
)

type bundleTransport func(*http.Request) (*http.Response, error)

func (f bundleTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBundleOutageRecoversBeyondFiveAttempts(t *testing.T) {
	root := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"layers":[]}`
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(manifest)))
		pins := agentlayer.Pins{{Name: "discobox-platform", Digest: digest}}
		var attempts int
		var waits []time.Duration
		last := time.Now()
		transport := bundleTransport(func(_ *http.Request) (*http.Response, error) {
			attempts++
			waits = append(waits, time.Since(last))
			last = time.Now()
			status, body := http.StatusServiceUnavailable, "pool upgrading"
			if attempts > 7 {
				status, body = http.StatusOK, manifest
			}
			return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		store := &bundlestore.Store{Root: root, Platform: imagecache.Platform{OS: "linux", Architecture: "amd64"}, Fetcher: bundlestore.HTTPFetcher{Transport: transport}}
		if err := applyBundles(context.Background(), pins, store); err != nil {
			t.Fatal(err)
		}
		if attempts != 8 {
			t.Fatalf("attempts = %d", attempts)
		}
		expected := []time.Duration{0, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
		for i, want := range expected {
			if waits[i] != want {
				t.Errorf("wait %d = %v, want %v", i, waits[i], want)
			}
		}
		trees, err := bundlestore.Trees(root)
		if err != nil || len(trees) != 1 {
			t.Fatalf("trees = %v, %v", trees, err)
		}
	})
}

func TestBundleRetryCancellationAndPermanentErrors(t *testing.T) {
	for _, scenario := range []string{"cancel", "404", "invalid content", "invalid pin"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				attempts := 0
				transport := bundleTransport(func(_ *http.Request) (*http.Response, error) {
					attempts++
					status := http.StatusOK
					switch scenario {
					case "cancel":
						status = http.StatusBadGateway
					case "404":
						status = http.StatusNotFound
					}
					return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader("bad content")), Header: make(http.Header)}, nil
				})
				store := &bundlestore.Store{Root: root, Fetcher: bundlestore.HTTPFetcher{Transport: transport}}
				pins := agentlayer.Pins{{Name: "discobox-platform", Digest: "sha256:" + strings.Repeat("a", 64)}}
				if scenario == "invalid pin" {
					pins[0].Digest = "invalid"
				}
				if scenario == "cancel" {
					time.AfterFunc(time.Second, cancel)
				}
				start := time.Now()
				err := applyBundles(ctx, pins, store)
				if err == nil {
					t.Fatal("wanted error")
				}
				if scenario == "cancel" {
					if !errors.Is(err, context.Canceled) || time.Since(start) != time.Second {
						t.Fatalf("cancellation = %v after %v", err, time.Since(start))
					}
				} else if time.Since(start) != 0 {
					t.Fatalf("permanent error retried: %v", err)
				}
				wantAttempts := 1
				if scenario == "invalid pin" {
					wantAttempts = 0
				}
				if attempts != wantAttempts {
					t.Fatalf("attempts = %d, want %d", attempts, wantAttempts)
				}
			})
		})
	}
}
