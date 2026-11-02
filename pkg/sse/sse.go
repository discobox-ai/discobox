// Package sse reads a server-sent events stream, the HTML format the server's
// event stream speaks (ADR 26-10-29-316). Shared because two clients follow
// that stream: the CLI's `discobox events`, and the `discobox-box` monitor
// in a box (ADR 26-11-01-560). It reads the format only; what an event's
// data means, and where a client resumes, are each caller's.
package sse

import (
	"bufio"
	"io"
	"strings"
)

// Read reads an event stream and hands each event to onEvent as it ends,
// until the stream ends or onEvent returns an error. The ID passed is the
// event's own, empty when it set none: the caller keeps the cursor. A block
// with an ID and nothing else is handed on with no type, as the cursor move
// it is: an EventSource takes its ID and dispatches nothing. A data-only
// block has the type `message`. Comments are skipped, and every other field,
// such as `retry`, goes to onField.
func Read(r io.Reader, onField func(field, value string), onEvent func(id, event, data string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var id, event string
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			switch {
			case len(data) > 0 || event != "":
				if event == "" {
					event = "message"
				}
				if err := onEvent(id, event, strings.Join(data, "\n")); err != nil {
					return err
				}
			case id != "":
				if err := onEvent(id, "", ""); err != nil {
					return err
				}
			}
			id, event, data = "", "", nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			id = value
		case "event":
			event = value
		case "data":
			data = append(data, value)
		default:
			if onField != nil {
				onField(field, value)
			}
		}
	}
	return scanner.Err()
}
