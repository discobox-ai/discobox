package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sort"
)

// AuthorizeRequest is the original request plus every credential it would use.
// Resolver implementations may inspect and replay Body, but must preserve its
// bytes and must never substitute a value during authorization.
type AuthorizeRequest struct {
	RequestID string
	ClientID  string
	Host      string
	Sentinels []string
	Request   *http.Request
}

func (s *Swapper) authorize(ctx context.Context, req *http.Request, clientID string) (string, error) {
	found := map[string]bool{}
	scan := func(value string) {
		swapSentinels(value, s.sentinels[clientID], func(sentinel string) (string, bool) { found[sentinel] = true; return sentinel, true })
	}
	for _, values := range req.Header {
		for _, v := range values {
			scan(v)
		}
	}
	if s.scanQuery && req.URL != nil {
		for _, values := range req.URL.Query() {
			for _, v := range values {
				scan(v)
			}
		}
	}
	if len(found) == 0 {
		return "", nil
	}
	sentinels := make([]string, 0, len(found))
	for sentinel := range found {
		sentinels = append(sentinels, sentinel)
	}
	sort.Strings(sentinels)
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	requestID := hex.EncodeToString(id[:])
	return requestID, s.resolver.Authorize(ctx, AuthorizeRequest{RequestID: requestID, ClientID: clientID, Host: extractHost(req.Host), Sentinels: sentinels, Request: req})
}
