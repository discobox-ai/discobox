package judge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Schema is what the wrapper is told the answer must look like. Decode is
// stricter than it, and the two are written together: the schema is what the
// model is aimed at, and Decode is what Discobox will actually accept.
const Schema = `{"type":"object","properties":{` +
	`"allow":{"type":"boolean"},` +
	`"reason":{"type":"string"},` +
	`"need":{"type":"object","properties":{"body":{"type":"string","enum":["text","json"]},"bytes":{"type":"integer"}},"required":["body"],"additionalProperties":false}` +
	`},"required":["reason"],"additionalProperties":false}`

// Answer is one round's reply. A judge either decides — allow, or not — or
// asks to be shown one more thing (ADR 0148 §6). Asking is not allowing: a
// job whose rounds run out without a decision is refused, like any other
// answer that is not an explicit allow.
type Answer struct {
	// Need is what the judge asked to be shown, when it asked instead of
	// deciding. Nil means it decided.
	Need *Need `json:"need,omitempty"`
	// Allow is the decision, and is meaningful only when Need is nil.
	Allow bool `json:"allow"`
	// Reason is always said, and is what the discobox is told when its
	// request is refused. It is the only thing it learns about why.
	Reason string `json:"reason"`
}

// Decided reports whether this answer settles the job.
func (a Answer) Decided() bool { return a.Need == nil }

// Need is the judge asking to be shown a request's body, in one form, with a
// budget it may name and Discobox caps.
type Need struct {
	Body  string `json:"body"`
	Bytes int    `json:"bytes,omitempty"`
}

// Budget is how much of the body this ask is answered with: what was asked
// for, never more than may ever be shown, and never nothing.
func (n Need) Budget() int {
	if n.Bytes <= 0 || n.Bytes > MaxBodyBytes {
		return MaxBodyBytes
	}
	return n.Bytes
}

// Decode accepts one complete, unambiguous answer and nothing else.
//
// It reads the answer token by token and names every field it will take,
// rather than unmarshalling into a struct: encoding/json matches field names
// case-insensitively, so a struct would read {"allow":false,"aLLoW":true} as
// an allow, and DisallowUnknownFields would not object because the second key
// matched the same field. The answer is model output that an injected request
// body can steer, so what may appear in it is spelled out here.
//
// Everything refused is something that could otherwise be read as an allow
// nobody gave: a transcript with a verdict somewhere in it, an object that
// both decides and asks, a key said twice in any spelling, a field nobody
// defined, text after the object. A wrapper that cannot produce exactly one
// answer has not answered.
func Decode(data []byte) (Answer, error) {
	if len(data) > MaxOutput {
		return Answer{}, errors.New("the judge's answer exceeds what one may be")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return Answer{}, errors.New("a verdict is one JSON object")
	}
	var (
		allow  *bool
		reason *string
		need   *Need
		seen   = map[string]bool{}
	)
	for decoder.More() {
		key, err := readKey(decoder, seen)
		if err != nil {
			return Answer{}, err
		}
		switch key {
		case "allow":
			value, err := readBool(decoder)
			if err != nil {
				return Answer{}, err
			}
			allow = &value
		case "reason":
			value, err := readString(decoder, "reason")
			if err != nil {
				return Answer{}, err
			}
			reason = &value
		case "need":
			value, err := readNeed(decoder)
			if err != nil {
				return Answer{}, err
			}
			need = value
		default:
			return Answer{}, fmt.Errorf("a verdict has no %q", key)
		}
	}
	if err := endObject(decoder); err != nil {
		return Answer{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return Answer{}, errors.New("the judge's answer carries more than a verdict")
	}
	if reason == nil || strings.TrimSpace(*reason) == "" {
		return Answer{}, errors.New("a verdict says why")
	}
	answer := Answer{Reason: strings.TrimSpace(*reason)}
	switch {
	case allow != nil && need != nil:
		return Answer{}, errors.New("a verdict decides or asks, never both")
	case allow != nil:
		answer.Allow = *allow
	case need != nil:
		answer.Need = need
	default:
		return Answer{}, errors.New("a verdict allows, refuses, or asks")
	}
	return answer, nil
}

// readNeed reads what the judge asked to be shown, which is a body in one of
// the two forms it may be written in.
func readNeed(decoder *json.Decoder) (*Need, error) {
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("an ask says what it needs to be shown")
	}
	var need Need
	seen := map[string]bool{}
	for decoder.More() {
		key, err := readKey(decoder, seen)
		if err != nil {
			return nil, err
		}
		switch key {
		case "body":
			form, err := readString(decoder, "body")
			if err != nil {
				return nil, err
			}
			if form != FormText && form != FormJSON {
				return nil, fmt.Errorf("a body is shown as %s or %s, not %q", FormText, FormJSON, form)
			}
			need.Body = form
		case "bytes":
			value, err := readInt(decoder)
			if err != nil {
				return nil, err
			}
			need.Bytes = value
		default:
			return nil, fmt.Errorf("an ask has no %q", key)
		}
	}
	if err := endObject(decoder); err != nil {
		return nil, err
	}
	if need.Body == "" {
		return nil, errors.New("an ask says what it needs to be shown")
	}
	return &need, nil
}

// readKey reads one field name, refusing a name already read in any spelling:
// keys are compared folded, because a reader that compares them byte for byte
// is a reader two spellings of the same field walk straight past.
func readKey(decoder *json.Decoder, seen map[string]bool) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", fmt.Errorf("the judge's answer is not a verdict: %w", err)
	}
	key, ok := token.(string)
	if !ok {
		return "", errors.New("the judge's answer is not a verdict")
	}
	folded := strings.ToLower(key)
	if seen[folded] {
		return "", fmt.Errorf("the judge's answer says %q twice", key)
	}
	seen[folded] = true
	return key, nil
}

func readBool(decoder *json.Decoder) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, fmt.Errorf("the judge's answer is not a verdict: %w", err)
	}
	value, ok := token.(bool)
	if !ok {
		return false, errors.New("allow is true or false")
	}
	return value, nil
}

func readString(decoder *json.Decoder, field string) (string, error) {
	token, err := decoder.Token()
	if err != nil {
		return "", fmt.Errorf("the judge's answer is not a verdict: %w", err)
	}
	value, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("%s is text", field)
	}
	return value, nil
}

func readInt(decoder *json.Decoder) (int, error) {
	token, err := decoder.Token()
	if err != nil {
		return 0, fmt.Errorf("the judge's answer is not a verdict: %w", err)
	}
	number, ok := token.(json.Number)
	if !ok {
		return 0, errors.New("bytes is a whole number")
	}
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, errors.New("bytes is a whole number")
	}
	return value, nil
}

// endObject reads the brace that closes an object whose members are done.
func endObject(decoder *json.Decoder) error {
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return errors.New("the judge's answer is not a verdict")
	}
	return nil
}
