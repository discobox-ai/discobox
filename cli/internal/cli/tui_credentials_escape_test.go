package cli

import (
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// What an agent wrote reaches the console escaped: a request's name,
// variable, routes and reason, and the variable and routes an approval copies
// onto a binding. The window draws what it is given (cli/REVIEW.md).
func TestTheConsoleGetsAgentTextEscaped(t *testing.T) {
	const evil = "\x1b]0;pwned\a"
	route := apimodel.SecretRoute{Host: "x" + evil + ".example", Path: apiclientgen.NewOptString("/p" + evil)}

	req := toTUICredentialRequest(apimodel.SecretRequest{
		ID:            "sreq_1",
		Name:          apiclientgen.NewOptString("gh" + evil),
		EnvName:       apiclientgen.NewOptString("GH" + evil),
		Justification: apiclientgen.NewOptString("line one\nline two" + evil),
		Routes:        []apimodel.SecretRoute{route},
	})
	binding := toTUIBinding(apimodel.SecretBinding{
		ID:     "sbd_1",
		EnvVar: apiclientgen.NewOptString("GH" + evil),
		Routes: []apimodel.SecretRoute{route},
	})

	for what, text := range map[string]string{
		"request name":     req.Name,
		"request variable": req.EnvVar,
		"request reason":   req.Justification,
		"request host":     req.Routes[0].Host,
		"request path":     req.Routes[0].Path,
		"binding variable": binding.EnvVar,
		"binding host":     binding.Routes[0].Host,
		"binding path":     binding.Routes[0].Path,
	} {
		if strings.ContainsAny(text, "\x1b\a") {
			t.Errorf("%s = %q, want it escaped", what, text)
		}
	}
	if !strings.Contains(req.Justification, "\n") {
		t.Errorf("reason = %q, want its line breaks kept", req.Justification)
	}
}
