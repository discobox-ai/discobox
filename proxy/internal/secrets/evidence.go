package secrets

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/judge"
)

type replayBody struct {
	io.Reader
	io.Closer
}

// Evidence captures a complete bounded request without changing the bytes the
// transport will send. A body we cannot inspect never authorizes substitution.
func (a AuthorizeRequest) Evidence(ctx context.Context) (string, error) {
	req := a.Request
	if req == nil || req.URL == nil {
		return "", fmt.Errorf("request evidence missing")
	}
	if req.Header.Get("Upgrade") != "" || req.Method == http.MethodConnect {
		return "", fmt.Errorf("use-scoped protocol upgrades cannot be judged")
	}
	clean := func(value string) string {
		return swapSentinels(value, a.Sentinels, func(string) (string, bool) { return "[credential]", true }).value
	}
	headers := http.Header{}
	for name, values := range req.Header {
		for _, value := range values {
			if credentialField(name) {
				headers.Add(name, "[redacted]")
			} else {
				headers.Add(name, clean(value))
			}
		}
	}
	u := *req.URL
	u.User = nil
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid request query")
	}
	for name, values := range query {
		for i, v := range values {
			if credentialField(name) {
				values[i] = "[redacted]"
			} else {
				values[i] = clean(v)
			}
		}
	}
	u.RawQuery = query.Encode()
	var body []byte
	if req.Body != nil && req.Body != http.NoBody {
		original := req.Body
		stop := context.AfterFunc(ctx, func() { _ = original.Close() })
		data, err := io.ReadAll(io.LimitReader(original, judge.MaxInput+1))
		stop()
		req.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(data), original), Closer: original}
		if err != nil {
			return "", fmt.Errorf("could not capture request body")
		}
		if len(data) > judge.MaxInput {
			return "", fmt.Errorf("request body exceeds judge limit")
		}
		body = data
		switch strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))) {
		case "", "identity":
		case "gzip":
			z, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return "", fmt.Errorf("invalid compressed request")
			}
			body, err = io.ReadAll(io.LimitReader(z, judge.MaxInput+1))
			_ = z.Close()
			if err != nil || len(body) > judge.MaxInput {
				return "", fmt.Errorf("compressed request exceeds inspection limit or is invalid")
			}
		default:
			return "", fmt.Errorf("unsupported request content encoding")
		}
	}
	if !utf8.Valid(body) {
		return "", fmt.Errorf("binary request body needs protocol-aware inspection")
	}
	if len(body) > 0 {
		kind, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
		if err != nil {
			return "", fmt.Errorf("request content type is required for inspection")
		}
		switch {
		case kind == "application/json" || strings.HasSuffix(kind, "+json"):
			d := json.NewDecoder(bytes.NewReader(body))
			d.UseNumber()
			value, err := inspectJSON(d, 0)
			if err != nil {
				return "", fmt.Errorf("invalid JSON request")
			}
			if err := d.Decode(new(any)); err != io.EOF {
				return "", fmt.Errorf("trailing JSON request content")
			}
			redactJSON(value)
			body, err = json.Marshal(value)
			if err != nil {
				return "", err
			}
		case kind == "application/x-www-form-urlencoded":
			values, err := url.ParseQuery(string(body))
			if err != nil {
				return "", fmt.Errorf("invalid form request")
			}
			for name := range values {
				if credentialField(name) {
					values.Set(name, "[redacted]")
				}
			}
			body = []byte(values.Encode())
		case strings.HasPrefix(kind, "text/"):
		default:
			return "", fmt.Errorf("request content type needs protocol-aware inspection")
		}
	}
	data, err := json.Marshal(struct {
		Method, Authority, URL string
		Headers                http.Header
		Body                   string
	}{req.Method, req.Host, clean(u.String()), headers, clean(string(body))})
	if err != nil {
		return "", err
	}
	if len(data) > judge.MaxInput {
		return "", fmt.Errorf("request evidence exceeds judge limit")
	}
	return string(data), nil
}

func credentialField(name string) bool {
	name = strings.ToLower(name)
	return strings.Contains(name, "authorization") || strings.Contains(name, "cookie") || strings.Contains(name, "token") || strings.Contains(name, "password") || strings.Contains(name, "secret") || name == "key" || strings.Contains(name, "api-key") || strings.Contains(name, "api_key") || name == "credential"
}
func redactJSON(value any) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if credentialField(key) {
				v[key] = "[redacted]"
			} else {
				redactJSON(child)
			}
		}
	case []any:
		for _, child := range v {
			redactJSON(child)
		}
	}
}

// Reject ambiguous objects: upstream parsers need not agree on which duplicate
// key wins. Preserve all array elements and number precision for the judge.
func inspectJSON(d *json.Decoder, depth int) (any, error) {
	if depth > 128 {
		return nil, fmt.Errorf("JSON nesting exceeds inspection limit")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("invalid JSON object key")
			}
			if _, exists := object[name]; exists {
				return nil, fmt.Errorf("duplicate JSON object key")
			}
			object[name], err = inspectJSON(d, depth+1)
			if err != nil {
				return nil, err
			}
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return object, nil
	case json.Delim('['):
		values := []any{}
		for d.More() {
			value, err := inspectJSON(d, depth+1)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		if _, err := d.Token(); err != nil {
			return nil, err
		}
		return values, nil
	default:
		if _, delim := token.(json.Delim); delim {
			return nil, fmt.Errorf("unexpected JSON delimiter")
		}
		return token, nil
	}
}
