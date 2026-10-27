package sandboxcreate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// secretNameHints are substrings that mark an --env variable name as sensitive,
// so its value is injected as a resolved secret rather than a plain environment
// variable. Use KEY!=VALUE to override.
var secretNameHints = []string{"KEY", "TOKEN", "PASS", "SECRET"}

// EnvAndSecretsFromOptions splits --env and --secret flags into the plain
// environment map and the inline secrets the create stores.
//
//   - --secret KEY[=VALUE|=@FILE][@HOST] stores a secret the create binds to
//     KEY. Its value is $KEY from this shell when nothing follows KEY, FILE's
//     contents (one trailing newline dropped) after =@, or VALUE itself — the
//     last resort, since a value on the command line reaches shell history
//     and the process list. It is sent anywhere (route `*`, ADR 26-10-27-939
//     §2) unless @HOST names where: HOST is what follows the last @, so a
//     value or path holding an @ of its own ends with one (KEY=me@example.com@).
//     An @ straight after = always means a file: a host cannot follow an empty
//     value. An existing secret is bound with --bind, never named here.
//   - --env KEY=VALUE / --env KEY is a plain variable, unless KEY looks sensitive
//     (contains KEY, TOKEN, PASS, or SECRET), in which case it becomes a secret.
//   - --env KEY!=VALUE forces a plain variable even when the name looks sensitive.
func EnvAndSecretsFromOptions(envArgs, secretArgs []string) (map[string]string, []apimodel.SandboxSecretInput, error) {
	env := map[string]string{}
	var secrets []apimodel.SandboxSecretInput
	seen := map[string]bool{}

	claim := func(key string) error {
		if seen[key] {
			return fmt.Errorf("duplicate variable %q across --env/--secret", key)
		}
		seen[key] = true
		return nil
	}

	for _, arg := range secretArgs {
		input, err := inlineSecret(arg)
		if err != nil {
			return nil, nil, err
		}
		if err := claim(input.Env); err != nil {
			return nil, nil, err
		}
		secrets = append(secrets, input)
	}

	for _, arg := range envArgs {
		keyTok, val, ok := strings.Cut(arg, "=")
		keyTok = strings.TrimSpace(keyTok)
		forcePlain := strings.HasSuffix(keyTok, "!")
		key := strings.TrimSpace(strings.TrimSuffix(keyTok, "!"))
		if key == "" {
			return nil, nil, fmt.Errorf("env must be in KEY=VALUE or KEY form")
		}
		value := val
		if !ok {
			shellValue, exists := os.LookupEnv(key)
			if !exists {
				continue
			}
			value = shellValue
		}
		if err := claim(key); err != nil {
			return nil, nil, err
		}
		if !forcePlain && keyLooksSecret(key) {
			secrets = append(secrets, apimodel.SandboxSecretInput{Env: key, Value: value})
			continue
		}
		env[key] = value
	}

	return env, secrets, nil
}

// inlineSecret reads one --secret argument, KEY[=VALUE|=@FILE][@HOST], into
// the inline secret a create stores.
func inlineSecret(arg string) (apimodel.SandboxSecretInput, error) {
	key, rest, hasValue := strings.Cut(arg, "=")
	fromFile := hasValue && strings.HasPrefix(rest, "@")
	if fromFile {
		rest = rest[1:]
	}
	if !hasValue {
		// KEY or KEY@HOST: the host is still what follows the last @.
		key, rest = splitHostless(key)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return apimodel.SandboxSecretInput{}, fmt.Errorf("secret must be KEY (from this shell), KEY=@FILE or KEY=VALUE, each with an optional @HOST; bind a stored secret with --bind SECRET[:VAR]")
	}
	text, host := rest, ""
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		parsed, err := inlineSecretHost(rest[at+1:])
		if err != nil {
			return apimodel.SandboxSecretInput{}, fmt.Errorf("--secret %s: %w; a value or path holding an @ ends with one, as in %s=me@example.com@", key, err, key)
		}
		text, host = rest[:at], parsed
	}
	var value string
	switch {
	case !hasValue:
		shellValue, ok := os.LookupEnv(key)
		if !ok {
			return apimodel.SandboxSecretInput{}, fmt.Errorf("--secret %s: %s is not set in this shell; export it, or read it from a file with %s=@FILE", key, key, key)
		}
		value = shellValue
	case fromFile:
		path := text
		if home, err := os.UserHomeDir(); err == nil && (path == "~" || strings.HasPrefix(path, "~/")) {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return apimodel.SandboxSecretInput{}, fmt.Errorf("--secret %s: read %s: %w", key, text, err)
		}
		value = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	default:
		value = text
	}
	if value == "" {
		return apimodel.SandboxSecretInput{}, fmt.Errorf("--secret %s: the value is empty", key)
	}
	input := apimodel.SandboxSecretInput{Env: key, Value: value}
	if host != "" {
		input.SetHost(apiclientgen.NewOptString(host))
	}
	return input, nil
}

// splitHostless splits KEY@HOST into the key and "@HOST", the shape the rest
// of the argument would have had after a value.
func splitHostless(arg string) (key, rest string) {
	if at := strings.LastIndex(arg, "@"); at >= 0 {
		return arg[:at], arg[at:]
	}
	return arg, ""
}

// inlineSecretHost reads the host after an inline secret's last @: empty is
// none, sent anywhere. A host is a name with a dot in it, localhost, or `*` —
// a word with no dot is far likelier the tail of a password than a host, and
// is refused rather than taken for one.
func inlineSecretHost(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	host, routePath, err := secretroute.Normalize(text, "")
	if err != nil || routePath != "" || (host != secretroute.AnyHost && host != "localhost" && !strings.Contains(host, ".")) {
		return "", fmt.Errorf("%q after the last @ is not a host", text)
	}
	return host, nil
}

func keyLooksSecret(key string) bool {
	upper := strings.ToUpper(key)
	for _, hint := range secretNameHints {
		if strings.Contains(upper, hint) {
			return true
		}
	}
	return false
}

// InlineSecretLine says where an inline secret will be sent, in the words a
// create prints: "DB_PASS: stored, sent to corp.example", or "sent anywhere".
// It is what shows a value's @ read as a host.
func InlineSecretLine(secret apimodel.SandboxSecretInput) string {
	if host, ok := secret.Host.Get(); ok && host != "" {
		return secret.Env + ": stored, sent to " + host
	}
	return secret.Env + ": stored, sent anywhere"
}
