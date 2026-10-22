package proxyagent

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/proxy"
)

// A git push over smart HTTP (ADR 26-09-26-240 §7).
//
// The body is the push's ref updates as pkt-lines — a four-digit hex length,
// then that many bytes less four — ended by a flush ("0000"); then, if the
// client asked for them, push options ended by another flush; then the
// packfile. The ref updates are a few hundred bytes of text that say exactly
// what the push does. The packfile is binary, and made the whole body read as
// "not text" before this: the judge was never shown which ref a push changes.

// gitReceivePack is a git push.
var gitReceivePack = &protocol{
	Recognition: judge.Recognition{Name: judge.ProtocolGitReceivePack, Version: 1},
	matches: func(req proxy.SecretAuthorizeRequest) bool {
		if req.Method != http.MethodPost {
			return false
		}
		return strings.HasSuffix(requestPath(req.URL), "/git-receive-pack") ||
			mediaTypeOf(req.Header) == "application/x-git-receive-pack-request"
	},
	parser: &bodyParser{
		Recognition: judge.Recognition{Name: judge.ParserGitReceivePack, Version: 1},
		describe:    describeGitPush,
		render:      renderGitPush,
	},
	refuse: refuseGitPush,
}

// gitPush is what a receive-pack request asks the server to do, read the way
// git's own server reads it (receive-pack's read_head_info): a judge shown
// anything else is judging a push the server will not apply.
type gitPush struct {
	commands     []gitCommand
	capabilities []string
	options      []string
	shallow      int
	signed       bool
	// probe is a body that is a flush and nothing else: what git sends
	// before a push too large to buffer, to learn whether the server will
	// take it. It changes nothing.
	probe bool
	// lines are the pkt-line payloads before the packfile, as sent, which
	// is what the judge is shown when it asks for the body.
	lines []string
	// pack is where the packfile starts, or -1 for a push that sends none
	// (one that only deletes).
	pack int
	// cut says the capture ended before the ref updates did.
	cut bool
}

// gitCommand is one ref update.
type gitCommand struct{ old, new, ref string }

// op is what an update does, read from its ids: an old id of all zeros makes
// a ref that does not exist yet, and a new id of zeros removes one.
func (c gitCommand) op() string {
	switch {
	case zeroID(c.old):
		return "create"
	case zeroID(c.new):
		return "delete"
	default:
		return "update"
	}
}

func zeroID(id string) bool { return strings.Trim(id, "0") == "" }

// gitCommandLine is one ref update: old id, new id, ref, the ids SHA-1's 40
// hex digits or SHA-256's 64. The ref is the rest of the line, as the server
// takes it.
var gitCommandLine = regexp.MustCompile(`^(?:([0-9a-f]{40}) ([0-9a-f]{40})|([0-9a-f]{64}) ([0-9a-f]{64})) (.+)$`)

func parseGitCommand(line string) (gitCommand, bool) {
	parts := gitCommandLine.FindStringSubmatch(line)
	if parts == nil {
		return gitCommand{}, false
	}
	if parts[1] != "" {
		return gitCommand{old: parts[1], new: parts[2], ref: parts[5]}, true
	}
	return gitCommand{old: parts[3], new: parts[4], ref: parts[5]}, true
}

// pktReader reads pkt-lines off a captured body.
type pktReader struct {
	data  []byte
	whole bool
	at    int
}

// next is the next pkt-line: its payload, or a flush. ok is false when the
// capture ended before a whole pkt-line did, which is a body cut short rather
// than a malformed one; for a whole body that is an error.
func (r *pktReader) next() (payload string, flush, ok bool, err error) {
	if len(r.data)-r.at < 4 {
		if r.whole {
			if r.at == len(r.data) {
				return "", false, false, errors.New("the body ends before its ref updates do")
			}
			return "", false, false, errors.New("the body ends inside a pkt-line length")
		}
		return "", false, false, nil
	}
	size, err := strconv.ParseUint(string(r.data[r.at:r.at+4]), 16, 16)
	if err != nil {
		if r.at == 0 {
			return "", false, false, errors.New("the body does not begin with a pkt-line, which every git push does")
		}
		return "", false, false, fmt.Errorf("the body has %q where a pkt-line length belongs", r.data[r.at:r.at+4])
	}
	if size == 0 {
		r.at += 4
		return "", true, true, nil
	}
	if size < 4 {
		return "", false, false, fmt.Errorf("the body has a pkt-line of length %d, which git never sends in a push", size)
	}
	if r.at+int(size) > len(r.data) {
		if r.whole {
			return "", false, false, errors.New("the body ends inside a pkt-line")
		}
		return "", false, false, nil
	}
	payload = string(r.data[r.at+4 : r.at+int(size)])
	r.at += int(size)
	return payload, false, true, nil
}

// parseGitPush reads the ref updates and what follows them. whole says data
// is the entire body; a body cut short is read as far as it goes.
func parseGitPush(data []byte, whole bool) (gitPush, error) {
	push := gitPush{pack: -1}
	reader := &pktReader{data: data, whole: whole}
	var cert strings.Builder
	for ended := false; !ended; {
		payload, flush, ok, err := reader.next()
		switch {
		case err != nil:
			return push, err
		case !ok:
			push.cut = true
			return push, nil
		case flush:
			ended = true
			continue
		}
		push.lines = append(push.lines, payload)
		line := strings.TrimSuffix(payload, "\n")
		if strings.HasPrefix(line, "shallow ") {
			push.shallow++
			continue
		}
		// The server reads capabilities after a NUL on any line, not only
		// the first, and the command is what comes before it.
		head, caps, found := strings.Cut(line, "\x00")
		if found {
			push.capabilities = append(push.capabilities, strings.Fields(caps)...)
		}
		if head == "push-cert" {
			// A signed push carries its ref updates inside the certificate,
			// which runs, newlines and all, to "push-cert-end" — or to a
			// flush, which also ends the ref updates.
			push.signed = true
			for {
				payload, flush, ok, err := reader.next()
				switch {
				case err != nil:
					return push, err
				case !ok:
					push.cut = true
					return push, nil
				}
				if flush {
					ended = true
					break
				}
				push.lines = append(push.lines, payload)
				// The server reads a certificate line as a C string: it
				// ends at a NUL, both where the line is compared and where
				// it is kept, so whatever follows one is nothing to git.
				if nul := strings.IndexByte(payload, 0); nul >= 0 {
					payload = payload[:nul]
				}
				if payload == "push-cert-end\n" {
					break
				}
				cert.WriteString(payload)
			}
			continue
		}
		command, ok := parseGitCommand(head)
		if !ok {
			return push, fmt.Errorf("line %d of the ref updates is not one", len(push.lines))
		}
		push.commands = append(push.commands, command)
	}
	if push.signed {
		if len(push.commands) > 0 {
			return push, errors.New("the push carries both a certificate and ref updates outside it, which git refuses")
		}
		commands, err := commandsFromCert(cert.String())
		if err != nil {
			return push, err
		}
		push.commands = commands
	}
	if push.hasCapability("push-options") {
		for {
			payload, flush, ok, err := reader.next()
			switch {
			case err != nil:
				return push, err
			case !ok:
				push.cut = true
				return push, nil
			case flush:
			default:
				push.lines = append(push.lines, payload)
				push.options = append(push.options, strings.TrimSuffix(payload, "\n"))
				continue
			}
			break
		}
	}
	if reader.at < len(data) {
		push.pack = reader.at
	}
	if len(push.commands) == 0 {
		if len(push.lines) == 0 && push.pack < 0 && whole {
			push.probe = true
			return push, nil
		}
		return push, errors.New("the push names no ref to update")
	}
	return push, nil
}

// commandsFromCert is the ref updates a push certificate carries: every line
// after its header, up to its signature, the way the server queues them.
func commandsFromCert(cert string) ([]gitCommand, error) {
	_, body, found := strings.Cut(cert, "\n\n")
	if !found {
		return nil, errors.New("the push certificate has no header, which git refuses")
	}
	if signature := signatureStart(body); signature >= 0 {
		body = body[:signature]
	}
	var commands []gitCommand
	for line := range strings.SplitSeq(strings.TrimSuffix(body, "\n"), "\n") {
		command, ok := parseGitCommand(line)
		if !ok {
			return nil, errors.New("a line of the push certificate is not a ref update, which git refuses")
		}
		commands = append(commands, command)
	}
	return commands, nil
}

// signatureStart is where the signature at the end of a certificate begins,
// found as git finds it: the last line that opens one.
func signatureStart(text string) int {
	start := -1
	for at := 0; at < len(text); {
		line := text[at:]
		for _, opener := range []string{"-----BEGIN PGP SIGNATURE-----", "-----BEGIN PGP MESSAGE-----",
			"-----BEGIN SIGNED MESSAGE-----", "-----BEGIN SSH SIGNATURE-----"} {
			if strings.HasPrefix(line, opener) {
				start = at
			}
		}
		next := strings.IndexByte(line, '\n')
		if next < 0 {
			break
		}
		at += next + 1
	}
	return start
}

func (p gitPush) hasCapability(name string) bool {
	for _, capability := range p.capabilities {
		if capability == name || strings.HasPrefix(capability, name+"=") {
			return true
		}
	}
	return false
}

// describeGitPush is what the first ask says about a push: every ref it
// changes, and how, which is the whole of what it does.
func describeGitPush(in parsedBody) (map[string]any, string) {
	push, err := parseGitPush(in.decoded, in.whole)
	if err != nil {
		return nil, err.Error()
	}
	if push.probe {
		return map[string]any{"probe": "the body is a flush and nothing else: git asking whether the server will take a push too large to buffer, which changes nothing"}, ""
	}
	commands := make([]any, 0, len(push.commands))
	for _, command := range push.commands {
		entry := map[string]any{"op": command.op(), "ref": in.redact(command.ref)}
		if command.op() != "create" {
			entry["old"] = shortID(command.old)
		}
		if command.op() != "delete" {
			entry["new"] = shortID(command.new)
		}
		commands = append(commands, entry)
	}
	metadata := map[string]any{"commands": commands}
	if push.cut {
		metadata["commandsCut"] = "the body is longer than this proxy reads, and the ref updates past it are not listed"
	}
	if len(push.capabilities) > 0 {
		// The client writes them, so they are redacted like anything else
		// it wrote.
		capabilities := make([]any, 0, len(push.capabilities))
		for _, capability := range push.capabilities {
			capabilities = append(capabilities, in.redact(capability))
		}
		metadata["capabilities"] = capabilities
	}
	if len(push.options) > 0 {
		options := make([]any, 0, len(push.options))
		for _, option := range push.options {
			options = append(options, in.redact(option))
		}
		metadata["pushOptions"] = options
	}
	if push.signed {
		metadata["signed"] = true
	}
	if push.shallow > 0 {
		metadata["shallow"] = push.shallow
	}
	if pack := packfileOf(push, in); pack != nil {
		metadata["packfile"] = pack
	}
	return metadata, ""
}

// packfileOf describes the packfile by its size and, when its header was
// read, how many objects it holds.
func packfileOf(push gitPush, in parsedBody) map[string]any {
	if push.pack < 0 {
		return nil
	}
	pack := map[string]any{}
	if in.whole {
		pack["bytes"] = len(in.decoded) - push.pack
	} else if in.length > int64(push.pack) { // -1, unknown, never is
		pack["bytes"] = in.length - int64(push.pack)
	}
	header := in.decoded[push.pack:]
	if len(header) >= 12 && bytes.HasPrefix(header, []byte("PACK")) {
		pack["objects"] = binary.BigEndian.Uint32(header[8:12])
	}
	return pack
}

// shortID is an object id as far as anyone reads one, which also keeps a
// push of many refs inside what metadata may say.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// renderGitPush is a push as the judge is shown it when it asks: what came
// before the packfile, as sent, one pkt-line to a line.
func renderGitPush(in parsedBody, budget int) (string, string) {
	push, err := parseGitPush(in.decoded, in.whole)
	var text strings.Builder
	for _, line := range push.lines {
		text.WriteString(strings.TrimSuffix(strings.ReplaceAll(line, "\x00", " "), "\n"))
		text.WriteByte('\n')
	}
	content, missing := cut(in.redact(text.String()), in.whole || push.pack >= 0, budget, "")
	var notes []string
	// The packfile is not said to be missing here: the metadata describes it
	// from the first ask, and a note on every showing would make an ask for
	// what was already shown look like progress (judge.Body.Answers).
	if err != nil {
		notes = append(notes, "the body stops being a git push: "+err.Error())
	}
	if missing != "" {
		notes = append(notes, missing)
	}
	return content, strings.Join(notes, "; ")
}

// refuseGitPush says no the way a git server does: a report that the pack was
// taken and every ref was rejected, with the reason. `git push` prints that as
// "! [remote rejected] <ref> (<reason>)", where a 403 it prints as
// "RPC failed; HTTP 403" and nothing else — which reads as a credential
// without permission, not a judge that said no.
//
// It needs the ref updates to name what it rejects, and a client that asked
// for a report; without both, the proxy's own refusal stands.
func refuseGitPush(body []byte, complete bool, said string) (proxy.SecretRefusal, bool) {
	push, err := parseGitPush(body, complete)
	if err != nil || len(push.commands) == 0 {
		return proxy.SecretRefusal{}, false
	}
	if !push.hasCapability("report-status") && !push.hasCapability("report-status-v2") {
		return proxy.SecretRefusal{}, false
	}
	// A reason is one line in a report, and a report line is a pkt-line.
	reason := strings.Join(strings.Fields(said), " ")
	if len(reason) > 900 {
		reason = reason[:900]
	}
	var report bytes.Buffer
	writePktLine(&report, []byte("unpack ok\n"))
	for _, command := range push.commands {
		writePktLine(&report, []byte("ng "+command.ref+" "+reason+"\n"))
	}
	report.WriteString("0000")

	band := 0
	switch {
	case push.hasCapability("side-band-64k"):
		band = 65515
	case push.hasCapability("side-band"):
		band = 995
	}
	if band == 0 {
		return proxy.SecretRefusal{Status: http.StatusOK, ContentType: "application/x-git-receive-pack-result", Body: report.Bytes()}, true
	}
	// Multiplexed: the whole sentence on the progress band, which git prints
	// as "remote: …" before anything else, then the report on the data band.
	var out bytes.Buffer
	writeSideBand(&out, 2, []byte(reason+"\n"), band)
	writeSideBand(&out, 1, report.Bytes(), band)
	out.WriteString("0000")
	return proxy.SecretRefusal{Status: http.StatusOK, ContentType: "application/x-git-receive-pack-result", Body: out.Bytes()}, true
}

func writePktLine(out *bytes.Buffer, payload []byte) {
	fmt.Fprintf(out, "%04x", len(payload)+4)
	out.Write(payload)
}

// writeSideBand writes data on one band, in packets no larger than the band
// the client asked for allows.
func writeSideBand(out *bytes.Buffer, band byte, data []byte, limit int) {
	for len(data) > 0 {
		n := min(len(data), limit)
		writePktLine(out, append([]byte{band}, data[:n]...))
		data = data[n:]
	}
}
