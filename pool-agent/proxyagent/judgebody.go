package proxyagent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/proxy"
)

// Showing the judge a body (ADR 26-09-22-838 §6).
//
// The first ask describes the body. When the judge answers that it needs to
// see it, the next ask carries it: in the form asked for, redacted, cut to the
// budget, and with a sentence saying what is not there and why. Nothing here
// changes what is sent — the proxy hands back every byte this read, in front
// of the rest.

// bodyArrivalWait bounds how long a round waits for a body the sandbox is
// still sending. The request is being held for a verdict, and a body that has
// not arrived by now is described as not having arrived rather than waited on.
const bodyArrivalWait = 10 * time.Second

// showBody is evidence answering need: the same request, with its body shown
// in the form the judge asked for or with why it cannot be.
func showBody(ctx context.Context, evidence *judge.Request, req proxy.SecretAuthorizeRequest, need judge.Need) *judge.Request {
	shown := *evidence
	body := judge.Body{MediaType: strings.TrimSpace(req.Header.Get("Content-Type"))}
	if evidence.Body != nil {
		body.MediaType, body.Length = evidence.Body.MediaType, evidence.Body.Length
	}
	shown.Body = &body

	wait, cancel := context.WithTimeout(ctx, bodyArrivalWait)
	defer cancel()
	raw, complete, err := req.Body.Capture(wait)
	if err != nil {
		// Nothing can be shown in any form, which is what an empty form says.
		if ctx.Err() == nil && wait.Err() != nil {
			body.Missing = fmt.Sprintf("the body had not arrived after %s", bodyArrivalWait)
		} else {
			body.Missing = "the body could not be read: " + err.Error()
		}
		return &shown
	}
	if complete || int64(len(raw)) > body.Length {
		// Measured now, which is better than what was declared: a chunked
		// body declared nothing, and a declared length is only a claim.
		body.Length = int64(len(raw))
	}

	decoded, decodedWhole, missing := decodeBody(raw, complete, req.Header.Get("Content-Encoding"))
	if missing != "" {
		body.Missing = missing
		return &shown
	}
	body.Form = need.Body
	if decodedWhole && len(decoded) == 0 {
		// Shown whole, and there is nothing in it.
		return &shown
	}
	budget := need.Budget()
	switch need.Body {
	case judge.FormJSON:
		body.Content, body.Missing = jsonForm(decoded, decodedWhole, budget, req.Sentinels)
	default:
		body.Content, body.Missing = textForm(decoded, decodedWhole, body.MediaType, budget, req.Sentinels)
	}
	return &shown
}

// decodeBody undoes the request's content encoding, so the judge reads what
// the upstream will. An encoding this does not decode shows nothing, and says
// which it was. It reports whether what it returns is the whole body.
func decodeBody(raw []byte, complete bool, encoding string) ([]byte, bool, string) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, complete, ""
	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, false, "the body says it is gzip-compressed and is not"
		}
		// Bounded like the capture, so a small body cannot decompress into a
		// large one on the request path.
		decoded, err := io.ReadAll(io.LimitReader(reader, proxy.MaxCapturedSecretBody+1))
		whole := complete && err == nil && len(decoded) <= proxy.MaxCapturedSecretBody
		if err != nil && complete {
			// All of it was read and it still would not decompress.
			return nil, false, "the body says it is gzip-compressed and is not"
		}
		if len(decoded) > proxy.MaxCapturedSecretBody {
			decoded = decoded[:proxy.MaxCapturedSecretBody]
		}
		return decoded, whole, ""
	default:
		return nil, false, fmt.Sprintf("the body is encoded as %q, which this proxy does not decode", encoding)
	}
}

// textForm is the body as sent. A form-encoded body has the value of every
// credential-looking parameter replaced, as the query string does, and every
// sentinel is taken out before anything is cut, so no cut can leave part of
// one behind.
func textForm(decoded []byte, whole bool, mediaType string, budget int, sentinels []string) (string, string) {
	if !whole {
		// A cut can fall inside a character, and that is the cut's doing, not
		// the body's.
		decoded = trimPartialRune(decoded)
	}
	if !utf8.Valid(decoded) {
		return "", "the body is not text"
	}
	text := string(decoded)
	if media, _, err := mime.ParseMediaType(mediaType); err == nil && media == "application/x-www-form-urlencoded" {
		text = redactedQuery(text)
	}
	text = redactSentinels(text, sentinels)
	return cut(text, whole, budget, "")
}

// jsonForm is the body parsed and written back as compact JSON, with the value
// under any key that reads like a credential replaced. It is written back from
// the token stream rather than from a decoded value, so a key said twice is
// shown twice and in order: a map would keep only one of them, and the
// upstream may read the other.
func jsonForm(decoded []byte, whole bool, budget int, sentinels []string) (string, string) {
	if !whole {
		return "", fmt.Sprintf("the body is more than the %d bytes this proxy reads, so it cannot be parsed as JSON; it can be shown as text", len(decoded))
	}
	if !utf8.Valid(decoded) {
		return "", "the body is not JSON"
	}
	var out bytes.Buffer
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.UseNumber()
	if err := writeJSONValue(dec, &out, false); err != nil {
		return "", "the body is not JSON"
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", "the body is not one JSON value"
	}
	return cut(redactSentinels(out.String(), sentinels), true, budget, "written back compacted, ")
}

// writeJSONValue copies one JSON value from dec to out, replacing every scalar
// inside it when redact is set.
func writeJSONValue(dec *json.Decoder, out *bytes.Buffer, redact bool) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		if redact {
			return writeJSONScalar(out, redactedValue)
		}
		return writeJSONScalar(out, token)
	}
	switch delim {
	case '{':
		out.WriteByte('{')
		for first := true; dec.More(); first = false {
			if !first {
				out.WriteByte(',')
			}
			key, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("an object key that is not a string")
			}
			if err := writeJSONScalar(out, name); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := writeJSONValue(dec, out, redact || credentialQueryParams[strings.ToLower(name)]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case '[':
		out.WriteByte('[')
		for first := true; dec.More(); first = false {
			if !first {
				out.WriteByte(',')
			}
			if err := writeJSONValue(dec, out, redact); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	default:
		return fmt.Errorf("unexpected %q", delim)
	}
	// The closing delimiter.
	_, err = dec.Token()
	return err
}

// writeJSONScalar writes one string, number, boolean or null. HTML is not
// escaped: the judge reads the characters that were sent, not <.
func writeJSONScalar(out *bytes.Buffer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return err
	}
	out.Truncate(out.Len() - 1) // Encode's newline
	return nil
}

// cut holds text to the budget, on a character boundary, and says what was
// left out: that the body was cut, how much of how much is shown, and how.
func cut(text string, whole bool, budget int, how string) (string, string) {
	if len(text) <= budget {
		if whole {
			return text, ""
		}
		return text, fmt.Sprintf("%sonly the first %d bytes are shown: the body is longer than this proxy reads", how, len(text))
	}
	shown := string(trimPartialRune([]byte(text[:budget])))
	if whole {
		return shown, fmt.Sprintf("%sonly the first %d of its %d bytes are shown", how, len(shown), len(text))
	}
	return shown, fmt.Sprintf("%sonly the first %d bytes are shown: the body is longer than this proxy reads", how, len(shown))
}

// trimPartialRune drops an incomplete UTF-8 sequence from the end, which is
// what cutting valid text at a byte count can leave.
func trimPartialRune(data []byte) []byte {
	for i := len(data) - 1; i >= 0 && i > len(data)-utf8.UTFMax; i-- {
		if utf8.RuneStart(data[i]) {
			if utf8.FullRune(data[i:]) {
				return data
			}
			return data[:i]
		}
	}
	return data
}
