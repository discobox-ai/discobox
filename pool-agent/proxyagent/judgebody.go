package proxyagent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"slices"
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
		// It can name the encoding the sandbox sent, and a sentinel in the
		// header is taken out of the sentence as it is out of the header.
		body.Missing = redactSentinels(missing, req.Sentinels)
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

// textForm is the body as sent. Every sentinel is taken out before anything is
// cut, so no cut can leave part of one behind, and a credential-named value is
// replaced wherever the body has names: a form-encoded body's, as the query
// string's are, and a JSON body's (redactedJSONText). Which is decided by the
// body, not by the form asked for — a judge shown JSON as text is shown what
// it would have been shown as JSON, less the compaction.
func textForm(decoded []byte, whole bool, mediaType string, budget int, sentinels []string) (string, string) {
	text, stopped, isText := redactedText(decoded, mediaType, whole, "the body", 0)
	if !isText {
		return "", "the body is not text"
	}
	text = redactSentinels(text, sentinels)
	content, missing := cut(text, whole || stopped != "", budget, "")
	switch {
	case stopped == "":
	case missing == "":
		missing = stopped
	default:
		missing = stopped + "; " + missing
	}
	return content, missing
}

// redactedText is text of a media type with every credential-named value
// replaced wherever that kind of text has names: form fields, JSON keys, and
// the parts of a multipart body, each of which is its own media type and is
// redacted as one. whole says data is all of it; stopped says what is not
// shown past a point the redaction could not follow, and isText is false for
// data that is not text at all. subject is what stopped calls the data, and
// depth is how many multipart bodies it is inside.
func redactedText(data []byte, mediaType string, whole bool, subject string, depth int) (text, stopped string, isText bool) {
	media, params, _ := mime.ParseMediaType(mediaType)
	if strings.HasPrefix(media, "multipart/") {
		if depth >= maxMultipartDepth {
			// Each level copies what is left of the body, and a body can nest
			// as deep as its bytes allow: past this it is described.
			return fmt.Sprintf("<%d bytes of multipart>", len(data)),
				fmt.Sprintf("%s is multipart inside multipart, which is described rather than shown", subject), true
		}
		// Read part by part, since one part that is not text — a file — does
		// not make the rest of the body unreadable.
		text, stopped = redactedMultipart(string(data), params["boundary"], whole, subject, depth)
		return text, stopped, true
	}
	if !whole {
		// A cut can fall inside a character, and that is the cut's doing, not
		// the body's.
		data = trimPartialRune(data)
	}
	if !utf8.Valid(data) {
		return "", "", false
	}
	text = string(data)
	switch {
	case media == "application/x-www-form-urlencoded":
		text = redactedQuery(text)
	case readsAsJSON(media, text):
		redacted, followed, cutShort := redactedJSONText(text)
		if followed < len(text) && (whole || !cutShort) {
			// Past here the text is not JSON, so nothing says which of what
			// follows is a credential's value, and none of it is shown.
			stopped = fmt.Sprintf("%s stops being JSON after its first %d bytes, and nothing after that is shown", subject, followed)
		}
		text = redacted
	}
	return text, stopped, true
}

// redactedMultipart is a multipart body part by part, each with the headers
// that say what it is and its content: a credential-named part's content is
// replaced, as a form field's value is, and a part that is not text is
// described by its length. The other part headers keep their names and lose
// their values, as a request's do (shownHeaders). It is written back rather
// than shown as sent, since only a part the reader has delimited can be
// redacted, and it stops at the first part that cannot be read; stopped says
// so when that is not the proxy's own read ending.
func redactedMultipart(text, boundary string, whole bool, subject string, depth int) (shown, stopped string) {
	if boundary == "" {
		return "", subject + " says it is multipart and names no boundary, so it is not shown"
	}
	var (
		out   strings.Builder
		notes []string
	)
	said := func(note string) string {
		if note != "" {
			notes = append(notes, note)
		}
		return strings.Join(notes, "; ")
	}
	reader := multipart.NewReader(strings.NewReader(text), boundary)
	for count := 1; ; count++ {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			fmt.Fprintf(&out, "--%s--\r\n", boundary)
			return out.String(), said("")
		}
		if err != nil {
			note := ""
			if whole {
				note = fmt.Sprintf("%s stops being multipart after %d parts, and nothing after that is shown", subject, count-1)
			}
			return out.String(), said(note)
		}
		fmt.Fprintf(&out, "--%s\r\n", boundary)
		names := slices.Sorted(maps.Keys(part.Header))
		for _, name := range names {
			for _, value := range part.Header[name] {
				if !shownPartHeaders[strings.ToLower(name)] {
					value = redactedValue
				}
				fmt.Fprintf(&out, "%s: %s\r\n", name, strings.ToValidUTF8(value, "\uFFFD"))
			}
		}
		out.WriteString("\r\n")
		content, readErr := io.ReadAll(part)
		// The name a part is filed under, whatever its disposition says it
		// is: FormName reads it only from form-data, and an attachment or a
		// part of multipart/mixed names itself the same way.
		_, disposition, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if credentialName(disposition["name"]) {
			out.WriteString(redactedValue)
		} else {
			// Each part is redacted as what it says it is, so a JSON or
			// form-encoded part loses the values a body of that type would.
			partText, partStopped, isText := redactedText(content, part.Header.Get("Content-Type"), readErr == nil, partName(subject, count), depth+1)
			switch {
			case !isText:
				fmt.Fprintf(&out, "<%d bytes that are not text>", len(content))
			default:
				out.WriteString(partText)
				if partStopped != "" {
					notes = append(notes, partStopped)
				}
			}
		}
		out.WriteString("\r\n")
		if readErr != nil {
			// The read ended inside this part: shown as far as it went.
			note := ""
			if whole {
				note = fmt.Sprintf("%s stops being multipart inside part %d, and nothing after that is shown", subject, count)
			}
			return out.String(), said(note)
		}
	}
}

// maxMultipartDepth is how many multipart bodies deep a part is shown: the
// body's own parts, and the parts of a multipart part one level down, which is
// as far as a form upload or a related-parts message goes.
const maxMultipartDepth = 2

// partName is what a part of subject is called: "part 3" of the body, and
// "part 3.2" of that part.
func partName(subject string, n int) string {
	if parent, ok := strings.CutPrefix(subject, "part "); ok {
		return fmt.Sprintf("part %s.%d", parent, n)
	}
	return fmt.Sprintf("part %d", n)
}

// shownPartHeaders are the part headers a judge is shown the value of: the
// ones that say what a part is.
var shownPartHeaders = map[string]bool{
	"content-disposition":       true,
	"content-type":              true,
	"content-transfer-encoding": true,
}

// readsAsJSON reports whether a body is to be redacted as JSON: it says it is,
// or it looks like it is. The look counts because the label is the sandbox's
// to write, and a JSON body labeled text/plain carries the same keys.
func readsAsJSON(media, text string) bool {
	if media == "application/json" || strings.HasSuffix(media, "+json") {
		return true
	}
	trimmed := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

// redactedJSONText is JSON text as it was sent, whitespace, order and repeated
// keys included, with every scalar under a credential-named key replaced where
// it stands. It follows the text as far as it reads as JSON and returns that
// much: followed is how many bytes of text it covers, and cutShort says the
// text ended inside a value rather than stopped being JSON. Whatever lies
// past followed is left out, because nothing there says which part is a
// credential's value.
func redactedJSONText(text string) (redacted string, followed int, cutShort bool) {
	// One frame per open object or array: whether everything in it is
	// redacted, and, for an object, whether a key comes next.
	type frame struct{ object, redact, wantKey bool }
	var (
		stack  []frame
		out    strings.Builder
		copied int  // text[:copied] is in out
		under  bool // the next value is a credential-named key's
	)
	valueDone := func() {
		under = false
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].wantKey = true
		}
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	for {
		start := int(dec.InputOffset())
		token, err := dec.Token()
		if err != nil {
			switch {
			case errors.Is(err, io.EOF) && len(stack) == 0 && !under:
				// Nothing but whitespace is left, and nothing is open.
				followed = len(text)
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				// Token says EOF between tokens even with an object open, and
				// ErrUnexpectedEOF inside one.
				cutShort = true
			}
			break
		}
		end := int(dec.InputOffset())
		followed = end
		top := (*frame)(nil)
		if n := len(stack); n > 0 {
			top = &stack[n-1]
		}
		switch t := token.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				stack = append(stack, frame{object: t == '{', redact: under || (top != nil && top.redact), wantKey: t == '{'})
				under = false
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		case string:
			if top != nil && top.object && top.wantKey {
				top.wantKey = false
				under = credentialName(t)
				continue
			}
		}
		if under || (top != nil && top.redact) {
			// The token's own bytes begin after the separators Token skipped.
			value := start + len(text[start:end]) - len(strings.TrimLeft(text[start:end], " \t\r\n,:"))
			out.WriteString(text[copied:value])
			out.WriteString(`"` + redactedValue + `"`)
			copied = end
		}
		valueDone()
	}
	out.WriteString(text[copied:followed])
	return out.String(), followed, cutShort
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
			if err := writeJSONValue(dec, out, redact || credentialName(name)); err != nil {
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
