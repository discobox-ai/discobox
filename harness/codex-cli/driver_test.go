package codexcli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
)

func TestDefinitionConfigure(t *testing.T) {
	def := Driver{}.Definition()
	if def.Configure == nil {
		t.Fatal("Configure = nil, want a configure spec")
	}
	scriptBytes, err := os.ReadFile("configure.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	if !strings.Contains(script, harness.ConfigureOutputPath) {
		t.Fatalf("configure script does not write the harness-configure.json contract: %s", script)
	}
	if !strings.Contains(script, harness.ConfigurePreviousConfigPath) {
		t.Fatalf("configure script ignores the seeded previous configuration: %s", script)
	}
	// Reuse goes through the PREV_ sentinel and comes back as usePrevious, so no
	// credential is read or re-emitted when the existing one is kept.
	if !strings.Contains(script, harness.ConfigurePreviousEnvPrefix+"$PREVIOUS_ENV") {
		t.Fatalf("configure script does not read the PREV_ sentinel: %s", script)
	}
	if !strings.Contains(script, "usePrevious") {
		t.Fatalf("configure script cannot report keeping the previous secret: %s", script)
	}
	// A ChatGPT sign-in (chosen from inside codex's own onboarding, not driven
	// by this script) yields a rotating token set, captured as an oauth secret
	// so the control plane can refresh the access token as it expires.
	if !strings.Contains(script, "refreshToken") || !strings.Contains(script, "'oauth'") {
		t.Fatalf("configure script does not capture a rotating oauth credential: %s", script)
	}
	if !strings.Contains(script, "auth.openai.com/oauth/token") {
		t.Fatalf("configure script does not record OpenAI's token endpoint: %s", script)
	}
	if !strings.Contains(script, "CODEX_OAUTH_TOKEN") || !strings.Contains(script, "OPENAI_API_KEY") {
		t.Fatalf("configure script does not offer both auth secrets: %s", script)
	}
	// The interactive TUI reads no credential environment variable at all, so
	// both shapes are delivered as the one file codex does read.
	if !strings.Contains(script, `.codex/auth.json`) {
		t.Fatalf("configure script does not deliver the credential as auth.json: %s", script)
	}
	// The credentials template is JSON, so a quote inside a template action
	// arrives at the renderer backslash-escaped and the parser rejects it.
	// Dotted field access has no quotes to escape; `index .secrets "NAME"` does,
	// and would break every sandbox launch while still letting configure report
	// success.
	if !strings.Contains(script, "{{ .secrets.${") {
		t.Fatalf("configure script does not build the credential template by field access: %s", script)
	}
	if strings.Contains(script, `index .secrets`) {
		t.Fatalf("configure script quotes a key inside a template action, which cannot survive JSON encoding: %s", script)
	}
	// codex must never rotate the delivered credential itself: the refresh token
	// stays in the control plane, so a rotation from inside a sandbox could not
	// succeed. A far-future last_refresh is what keeps it from trying.
	if !strings.Contains(script, "AUTH_LAST_REFRESH") {
		t.Fatalf("configure script lets codex decide the delivered credential is stale: %s", script)
	}
	if !strings.Contains(script, "codex exec") {
		t.Fatalf("configure script does not verify the credential with a test prompt: %s", script)
	}
	// The configure sandbox has no source, so the image's config.toml template
	// trusts nothing; the script must trust the workspace itself or the
	// interactive session stops at the trust screen instead of the sign-in one.
	if !strings.Contains(script, "ensure_workspace_trusted") || !strings.Contains(script, `trust_level = "trusted"`) {
		t.Fatalf("configure script does not pre-trust the workspace for sign-in: %s", script)
	}
	// Settings and directory trust share one file in codex, and this throwaway
	// sandbox's trust map must not become the harness's, so [projects] is
	// stripped from what is captured and one templated stanza put back.
	if !strings.Contains(script, "startsWith('projects.')") {
		t.Fatalf("configure script returns the configure sandbox's own trust map: %s", script)
	}
	// Every retry is gated on a person asking for one. An attempt that fails
	// without ever reaching the user -- codex refusing to start, say -- fails
	// again the instant it is retried, so an ungated `continue` is a busy loop
	// rather than a retry.
	if !strings.Contains(script, "confirm_retry") {
		t.Fatalf("configure script retries without asking, so a failing launch spins: %s", script)
	}
	loopStart := strings.Index(script, "while [ -z \"$ENV_NAME\" ]; do")
	if loopStart < 0 {
		t.Fatal("configure script has no credential-collection loop")
	}
	loop := script[loopStart:]
	if loopEnd := strings.Index(loop, "\ndone\n"); loopEnd >= 0 {
		loop = loop[:loopEnd]
	}
	// Split on `continue`: every segment but the last is the code leading up to
	// one, and each has to have asked before taking it.
	segments := strings.Split(loop, "continue")
	for _, leadingUpToAContinue := range segments[:len(segments)-1] {
		if !strings.Contains(leadingUpToAContinue, "confirm_retry") {
			t.Fatalf("configure script's loop continues without confirm_retry, so it can spin: %s", script)
		}
	}
	// codex draws a full-screen TUI, so the banner has to be acknowledged before
	// the launch or the user never sees it.
	if !strings.Contains(script, "confirm_launch") {
		t.Fatalf("configure script launches codex without holding its instructions on screen: %s", script)
	}
	// This sandbox has no browser, so the one sign-in that works here is named
	// explicitly; /exit is what setup cannot finish without; /logout is how an
	// already-signed-in session switches accounts, codex having no /login.
	for _, phrase := range []string{"Device Code", "/exit", "/model", "/logout"} {
		if !strings.Contains(script, phrase) {
			t.Fatalf("configure script does not point the user at %s: %s", phrase, script)
		}
	}
	// Emphasis is opt-out, not unconditional: this output is also read from a
	// log, and a hardcoded escape sequence would land there too.
	if strings.Contains(script, `\033[`) && !strings.Contains(script, "NO_COLOR") {
		t.Fatalf("configure script colorizes without honoring NO_COLOR: %s", script)
	}
	// Reconfigure opens the session already signed in, so it can be used to
	// change a setting without re-authenticating.
	if !strings.Contains(script, "seed_previous_credential") {
		t.Fatalf("configure script does not sign the reconfigure session in: %s", script)
	}
	// What was seeded is a sentinel we chose, so finding it still in place is
	// what proves nothing re-authenticated -- that comparison is the whole
	// change check, and without it every reconfigure would store a credential.
	if !strings.Contains(script, `!= "$SEEDED_SENTINEL"`) {
		t.Fatalf("configure script does not detect auth changes by comparison: %s", script)
	}
	// The launch's exit status is what separates "you did not sign in" from
	// "codex would not run", and the retry prompt is useless if the script
	// cannot tell the user which happened.
	if !strings.Contains(script, "codex_status") {
		t.Fatalf("configure script ignores whether codex actually ran: %s", script)
	}
}

// TestImageDeclaresFileDeliveredAuth pins the half of the contract the image
// owns: codex authenticates from a file, so neither credential may be exported
// as an environment variable, and each is an alternative to the other.
func TestImageDeclaresFileDeliveredAuth(t *testing.T) {
	raw, err := os.ReadFile("image.json")
	if err != nil {
		t.Fatal(err)
	}
	var image struct {
		Harness struct {
			Files   []harness.File `json:"files"`
			Secrets []struct {
				Name       string `json:"name"`
				Required   bool   `json:"required"`
				OneOfGroup string `json:"oneOfGroup"`
				Delivery   string `json:"delivery"`
			} `json:"secrets"`
		} `json:"harness"`
	}
	if err := json.Unmarshal(raw, &image); err != nil {
		t.Fatal(err)
	}
	wantSecrets := map[string]bool{"OPENAI_API_KEY": false, "CODEX_OAUTH_TOKEN": false}
	for _, secret := range image.Harness.Secrets {
		if _, ok := wantSecrets[secret.Name]; !ok {
			continue
		}
		wantSecrets[secret.Name] = true
		if secret.Delivery != harness.SecretDeliveryFile {
			t.Errorf("secret %s delivery = %q, want %q: codex's interactive TUI reads no credential env var",
				secret.Name, secret.Delivery, harness.SecretDeliveryFile)
		}
		if !secret.Required || secret.OneOfGroup == "" {
			t.Errorf("secret %s is not a required alternative: required=%v group=%q", secret.Name, secret.Required, secret.OneOfGroup)
		}
	}
	for name, found := range wantSecrets {
		if !found {
			t.Errorf("image declares no %s secret", name)
		}
	}
	// The credential file is the configure flow's to write. A baseline one
	// would land in every sandbox as a credential-shaped file with nothing
	// behind it, which codex reads as "signed in" -- and then fails every
	// request with no way to sign in from the session.
	for _, file := range image.Harness.Files {
		if file.Path == ".codex/auth.json" {
			t.Errorf("image declares a baseline %s, which authenticates a sandbox with nothing", file.Path)
		}
	}
}
