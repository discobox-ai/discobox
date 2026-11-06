package proxyagent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/discobox-ai/discobox/proxy"
)

func TestGateDialRetriesOnlyListenerReplacementErrors(t *testing.T) {
	for _, failure := range []error{syscall.ENOENT, gateConnectionRefused, syscall.EACCES, syscall.EHOSTUNREACH, io.EOF} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := 0
				client, server := net.Pipe()
				defer client.Close()
				defer server.Close()
				dial := gateDialContext(func(context.Context, string, string) (net.Conn, error) {
					attempts++
					if attempts == 1 {
						return nil, &net.OpError{Op: "dial", Err: failure}
					}
					return client, nil
				})
				start := time.Now()
				conn, err := dial(t.Context(), "tcp", "unused")
				retry := errors.Is(failure, syscall.ENOENT) || errors.Is(failure, gateConnectionRefused)
				if retry {
					if err != nil || conn != client || attempts != 2 || time.Since(start) != gateConnectRetry {
						t.Fatalf("conn=%v error=%v attempts=%d elapsed=%v", conn, err, attempts, time.Since(start))
					}
				} else if !errors.Is(err, failure) || attempts != 1 || time.Since(start) != 0 {
					t.Fatalf("error=%v attempts=%d elapsed=%v", err, attempts, time.Since(start))
				}
			})
		})
	}
}

func TestGateDialWaitIsBoundedAndCancelable(t *testing.T) {
	for _, cancelAfter := range []time.Duration{0, 250 * time.Millisecond, gateConnectWait * 2} {
		t.Run(cancelAfter.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if cancelAfter == 0 {
					cancel()
				} else {
					time.AfterFunc(cancelAfter, cancel)
				}
				attempts := 0
				dial := gateDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
					attempts++
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					return nil, gateConnectionRefused
				})
				start := time.Now()
				_, err := dial(ctx, "tcp", "unused")
				want, elapsed := context.Canceled, cancelAfter
				if cancelAfter > gateConnectWait {
					want, elapsed = context.DeadlineExceeded, gateConnectWait
				}
				if !errors.Is(err, want) || time.Since(start) != elapsed {
					t.Fatalf("error=%v elapsed=%v attempts=%d", err, time.Since(start), attempts)
				}
			})
		})
	}
}

func TestGateDialBudgetIncludesTimeInsideDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		dial := gateDialContext(func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		_, err := dial(t.Context(), "tcp", "unused")
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != gateConnectWait {
			t.Fatalf("error=%v elapsed=%v", err, time.Since(start))
		}
	})
}

func TestGateDoesNotReplayTransmittedPOST(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "status", true: "disconnect"}[disconnect], func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
					return
				}
				requests.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "one upload" {
					t.Errorf("body=%q err=%v", body, err)
				}
				if disconnect {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				} else {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			t.Setenv(envControlPlaneURL, server.URL)
			root := withTestRoot(t)
			if err := WriteResolveContext(root, testProjectID, testPoolID, server.URL, "pool-token"); err != nil {
				t.Fatal(err)
			}
			resolver := newSecretResolver(root, testProjectID, testPoolID)
			in := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+GateHost+"/projects/default/sandboxes", io.NopCloser(strings.NewReader("one upload")))
			admitted, err := resolver.Gate(t.Context(), proxy.SecretGateRequest{ClientID: "sb-1", Request: in})
			if disconnect {
				if err == nil {
					admitted.Response.Body.Close()
					t.Fatal("expected disconnect error")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				admitted.Response.Body.Close()
				if admitted.Response.StatusCode != http.StatusServiceUnavailable {
					t.Fatal(admitted.Response.Status)
				}
			}
			if requests.Load() != 1 {
				t.Fatalf("received %d requests; want 1", requests.Load())
			}
		})
	}
}
