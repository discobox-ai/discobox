package sandboxcreate

import (
	"fmt"
	"os"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// secretNameHints are substrings that mark an --env variable name as sensitive,
// so its value is injected as a resolved secret rather than a plain environment
// variable. Use KEY!=VALUE to override.
var secretNameHints = []string{"KEY", "TOKEN", "PASS", "SECRET"}

// EnvAndSecretsFromOptions splits --env and --secret flags into the plain
// environment map and the secret assignment inputs.
//
//   - --secret KEY=VALUE injects an inline value (an anonymous secret is created).
//   - --secret KEY=<SECRET_ID> references an existing secret by ID.
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
		key, val, ok := strings.Cut(arg, "=")
		key = strings.TrimSpace(key)
		if key == "" || !ok {
			return nil, nil, fmt.Errorf("secret must be in KEY=VALUE or KEY=<SECRET_ID> form")
		}
		if err := claim(key); err != nil {
			return nil, nil, err
		}
		input := apimodel.SandboxSecretInput{Env: key}
		if ref, isRef := secretIDReference(val); isRef {
			input.SetSecretId(apiclientgen.NewOptString(ref))
		} else {
			input.SetValue(apiclientgen.NewOptString(val))
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
			input := apimodel.SandboxSecretInput{Env: key}
			input.SetValue(apiclientgen.NewOptString(value))
			secrets = append(secrets, input)
			continue
		}
		env[key] = value
	}

	return env, secrets, nil
}

// secretIDReference reports whether val is a KEY=<SECRET_ID> reference and
// returns the inner ID.
func secretIDReference(val string) (string, bool) {
	if len(val) >= 2 && strings.HasPrefix(val, "<") && strings.HasSuffix(val, ">") {
		return strings.TrimSpace(val[1 : len(val)-1]), true
	}
	return "", false
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
