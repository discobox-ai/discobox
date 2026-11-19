package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"unicode/utf8"
)

const maxClipboardBytes = 64 * 1024

// A missing text field reads the desktop selection. An empty text field sets
// an empty selection. JSON makes cross-origin browser requests need a preflight.
func (s *Server) serveClipboard(w http.ResponseWriter, r *http.Request) {
	// The clipboard API also refuses arbitrary DNS names, which a browser
	// could rebind. A forward can be reached by localhost or its numeric IP.
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = strings.Trim(r.Host, "[]")
	}
	if !strings.EqualFold(host, "localhost") && net.ParseIP(host) == nil &&
		(s.listenHost == "" || !strings.EqualFold(host, s.listenHost)) {
		s.fail(w, http.StatusForbidden, errors.New("desktop: open the viewer using localhost or the forwarded IP address to use the clipboard"))
		return
	}
	if !s.checkViewerOrigin(w, r) {
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		s.fail(w, http.StatusUnsupportedMediaType, errors.New("desktop: clipboard requires JSON"))
		return
	}
	var request struct {
		Text *string `json:"text"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 6*maxClipboardBytes+1024)
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		s.fail(w, http.StatusBadRequest, errors.New("desktop: invalid clipboard request"))
		return
	}
	if request.Text != nil {
		text := *request.Text
		if len(text) > maxClipboardBytes {
			text = ""
		}
		if err := s.display.setClipboard(r.Context(), text); err != nil {
			s.fail(w, http.StatusServiceUnavailable, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	text, err := s.display.clipboard(r.Context())
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func (d *Display) setClipboard(ctx context.Context, text string) error {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-in", "-silent")
	cmd.Env = d.env()
	cmd.Stdin = strings.NewReader(text)
	// xclip forks to own the selection until a different client takes it.
	if err := cmd.Run(); err != nil {
		return errors.New("desktop: could not set the clipboard")
	}
	return nil
}

func (d *Display) clipboard(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-out")
	cmd.Env = d.env()
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", errors.New("desktop: could not open the clipboard")
	}
	if err := cmd.Start(); err != nil {
		pipe.Close()
		return "", errors.New("desktop: could not start the clipboard reader")
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, maxClipboardBytes+1))
	if len(data) > maxClipboardBytes || readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if len(data) > maxClipboardBytes {
		return "", errors.New("desktop: clipboard text is too large")
	}
	if readErr != nil || waitErr != nil || !utf8.Valid(data) {
		return "", errors.New("desktop: clipboard has no available plain text")
	}
	return string(data), nil
}
