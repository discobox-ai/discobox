package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	envAgents      = "DISCOBOX_PROMPTER_E2E_AGENTS"
	envCacheDir    = "DISCOBOX_PROMPTER_E2E_CACHE_DIR"
	envList        = "DISCOBOX_PROMPTER_E2E_LIST"
	envPrompt      = "DISCOBOX_PROMPTER_E2E_PROMPT"
	envSkipInstall = "DISCOBOX_PROMPTER_E2E_SKIP_INSTALL"
	envTimeout     = "DISCOBOX_PROMPTER_E2E_TIMEOUT"
	envUpdate      = "DISCOBOX_PROMPTER_E2E_UPDATE"
)

type agentCase struct {
	Name        string
	EnvKey      string
	Expected    string
	Credentials credentialRequirement
	Install     func(context.Context, *testing.T, string, string) string
	RunCommand  func(string) []string
	AgentEnv    func() map[string]string
}

type credentialRequirement struct {
	EnvVars []string
	Notes   string
}

func TestAgentDetectorE2E(t *testing.T) {
	cases := e2eCases()
	if os.Getenv(envList) == "1" {
		printCredentialRequirements(t, cases)
		return
	}
	enabled := enabledAgents(t, cases)
	if len(enabled) == 0 {
		t.Skipf("set %s to a comma-separated list or all to run real agent detector e2e tests", envAgents)
	}

	workspace := t.TempDir()
	prompter := buildPrompter(t, workspace)
	cacheDir := e2eCacheDir(t)
	t.Logf("using e2e cache directory: %s", cacheDir)

	for _, tc := range enabled {
		t.Run(tc.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), e2eTimeout(t))
			defer cancel()

			requireCredentials(t, tc)
			agentBin := tc.Install(ctx, t, cacheDir, tc.EnvKey)
			prompt := detectionPrompt(prompter)
			output := runAgent(ctx, t, workspace, tc, agentBin, prompt)
			if !strings.Contains(output, tc.Expected) {
				t.Fatalf("expected output to contain %q, got:\n%s", tc.Expected, output)
			}
		})
	}
}

func e2eCases() []agentCase {
	return []agentCase{
		{
			Name:     "claude-code",
			EnvKey:   "CLAUDE_CODE",
			Expected: "claude-code",
			Credentials: credentialRequirement{
				EnvVars: []string{"ANTHROPIC_API_KEY"},
				Notes:   "Claude Code is provider-specific; use Anthropic auth for this case.",
			},
			Install: npmInstaller("@anthropic-ai/claude-code", "claude"),
			RunCommand: func(prompt string) []string {
				return []string{"--bare", "--permission-mode", "bypassPermissions", "-p", prompt, "--output-format", "text"}
			},
		},
		{
			Name:     "codex",
			EnvKey:   "CODEX",
			Expected: "codex",
			Credentials: credentialRequirement{
				EnvVars: []string{"OPENAI_API_KEY"},
				Notes:   "Codex exec expects the API key in CODEX_API_KEY for a single non-interactive run.",
			},
			Install: npmInstaller("@openai/codex", "codex"),
			RunCommand: func(prompt string) []string {
				return []string{"exec", "--ephemeral", "--skip-git-repo-check", "--sandbox", "danger-full-access", prompt}
			},
			AgentEnv: func() map[string]string {
				return map[string]string{
					"CODEX_API_KEY": os.Getenv("OPENAI_API_KEY"),
				}
			},
		},
		{
			Name:     "gemini-cli",
			EnvKey:   "GEMINI",
			Expected: "gemini-cli",
			Credentials: credentialRequirement{
				EnvVars: []string{"GEMINI_API_KEY"},
				Notes:   "Gemini CLI is provider-specific; this does not use OPENAI_API_KEY.",
			},
			Install: npmInstaller("@google/gemini-cli", "gemini"),
			RunCommand: func(prompt string) []string {
				return []string{"--skip-trust", "-y", "-p", prompt}
			},
		},
		{
			Name:     "opencode",
			EnvKey:   "OPENCODE",
			Expected: "opencode",
			Credentials: credentialRequirement{
				EnvVars: []string{"OPENAI_API_KEY"},
				Notes:   "OpenCode can use OpenAI-compatible providers; the default e2e path assumes OpenAI.",
			},
			Install: npmInstaller("opencode-ai", "opencode"),
			RunCommand: func(prompt string) []string {
				return []string{"run", prompt}
			},
		},
		{
			Name:     "discobot",
			EnvKey:   "DISCOBOT",
			Expected: "discobot",
			Credentials: credentialRequirement{
				EnvVars: []string{"OPENAI_API_KEY"},
				Notes:   "Discobot uses the local disco CLI with an OpenAI-backed model for this case.",
			},
			Install: pathOnlyInstaller("disco"),
			RunCommand: func(prompt string) []string {
				return []string{"-p", "-model", discobotOpenAIModel(), prompt}
			},
		},
	}
}

func enabledAgents(t *testing.T, cases []agentCase) []agentCase {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(envAgents))
	if value == "" {
		return nil
	}
	byName := make(map[string]agentCase, len(cases))
	for _, tc := range cases {
		byName[tc.Name] = tc
	}
	if strings.EqualFold(value, "all") {
		return cases
	}

	var enabled []agentCase
	for _, item := range strings.Split(value, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		tc, ok := byName[name]
		if !ok {
			var names []string
			for known := range byName {
				names = append(names, known)
			}
			sort.Strings(names)
			t.Fatalf("unknown e2e agent %q in %s; known agents: %s", name, envAgents, strings.Join(names, ", "))
		}
		enabled = append(enabled, tc)
	}
	return enabled
}

func requireCredentials(t *testing.T, tc agentCase) {
	t.Helper()
	var missing []string
	for _, envVar := range tc.Credentials.EnvVars {
		if strings.TrimSpace(os.Getenv(envVar)) == "" {
			missing = append(missing, envVar)
		}
	}
	if len(missing) == 0 {
		return
	}
	t.Fatalf("missing credential environment for %s: set %s. %s", tc.Name, strings.Join(missing, ", "), tc.Credentials.Notes)
}

func printCredentialRequirements(t *testing.T, cases []agentCase) {
	t.Helper()
	fmt.Fprintln(os.Stderr, "prompter e2e credential requirements:")
	for _, tc := range cases {
		fmt.Fprintf(os.Stderr, "- %s: %s", tc.Name, strings.Join(tc.Credentials.EnvVars, ", "))
		if tc.Credentials.Notes != "" {
			fmt.Fprintf(os.Stderr, " — %s", tc.Credentials.Notes)
		}
		fmt.Fprintln(os.Stderr)
	}
}

func buildPrompter(t *testing.T, workspace string) string {
	t.Helper()
	moduleRoot := prompterModuleRoot(t)
	output := filepath.Join(workspace, "prompter")
	cmd := exec.Command("go", "build", "-o", output, "./cmd/prompter")
	cmd.Dir = moduleRoot
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build prompter: %v\n%s", err, combined)
	}
	return output
}

func prompterModuleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func e2eCacheDir(t *testing.T) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv(envCacheDir)); value != "" {
		mkdirAll(t, value)
		return value
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("resolve user cache dir: %v", err)
	}
	dir := filepath.Join(base, "discobox", "prompter-e2e")
	mkdirAll(t, dir)
	return dir
}

func e2eTimeout(t *testing.T) time.Duration {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(envTimeout))
	if value == "" {
		return 10 * time.Minute
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", envTimeout, value, err)
	}
	return duration
}

func npmInstaller(packageName, binName string) func(context.Context, *testing.T, string, string) string {
	return func(ctx context.Context, t *testing.T, cacheDir string, envKey string) string {
		t.Helper()
		if bin := overriddenBin(t, envKey); bin != "" {
			return bin
		}
		if os.Getenv(envSkipInstall) == "1" {
			return lookPath(t, binName)
		}

		installDir := filepath.Join(cacheDir, "node", safePathName(packageName))
		bin := filepath.Join(installDir, "node_modules", ".bin", binName)
		if runtime.GOOS == "windows" {
			bin += ".cmd"
		}
		if os.Getenv(envUpdate) != "1" && fileExists(bin) {
			runNodePostInstall(ctx, t, installDir, packageName)
			return bin
		}

		mkdirAll(t, installDir)
		if pnpm, ok := lookPathOptional("pnpm"); ok {
			runInstall(ctx, t, installDir, pnpm, []string{"--dir", installDir, "add", "--allow-build=@github/keytar", "--allow-build=node-pty", "--allow-build=protobufjs", "--allow-build=" + packageName, packageName}, map[string]string{
				"PNPM_HOME":        filepath.Join(cacheDir, "pnpm-home"),
				"XDG_CACHE_HOME":   filepath.Join(cacheDir, "xdg-cache"),
				"npm_config_cache": filepath.Join(cacheDir, "npm-cache"),
			})
		} else if npm, ok := lookPathOptional("npm"); ok {
			runInstall(ctx, t, installDir, npm, []string{"install", "--no-audit", "--no-fund", "--prefix", installDir, packageName}, map[string]string{
				"npm_config_cache": filepath.Join(cacheDir, "npm-cache"),
			})
		} else {
			t.Fatalf("pnpm or npm is required to install %s", packageName)
		}
		runNodePostInstall(ctx, t, installDir, packageName)
		return bin
	}
}

func runNodePostInstall(ctx context.Context, t *testing.T, installDir string, packageName string) {
	t.Helper()
	node, ok := lookPathOptional("node")
	if !ok {
		return
	}
	switch packageName {
	case "@anthropic-ai/claude-code":
		script := filepath.Join(installDir, "node_modules", packageName, "install.cjs")
		if fileExists(script) {
			runInstall(ctx, t, installDir, node, []string{script}, nil)
		}
	case "opencode-ai":
		script := filepath.Join(installDir, "node_modules", packageName, "postinstall.mjs")
		if fileExists(script) {
			runInstall(ctx, t, installDir, node, []string{script}, nil)
		}
	}
}

func pathOnlyInstaller(binName string) func(context.Context, *testing.T, string, string) string {
	return func(_ context.Context, t *testing.T, _ string, _ string) string {
		t.Helper()
		return lookPath(t, binName)
	}
}

func runInstall(ctx context.Context, t *testing.T, dir string, name string, args []string, extraEnv map[string]string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = mergedEnv(extraEnv)
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install command failed: %s %s: %v\n%s", name, strings.Join(args, " "), err, combined)
	}
}

func runAgent(ctx context.Context, t *testing.T, workspace string, tc agentCase, agentBin string, prompt string) string {
	t.Helper()
	pathPrepend := filepath.Dir(agentBin)
	if template := strings.TrimSpace(os.Getenv("DISCOBOX_PROMPTER_E2E_" + tc.EnvKey + "_COMMAND")); template != "" {
		command := strings.ReplaceAll(template, "{prompt}", shellQuote(prompt))
		command = strings.ReplaceAll(command, "{prompter}", shellQuote(filepath.Join(workspace, "prompter")))
		return runCombined(ctx, t, workspace, tc, "/bin/sh", []string{"-c", command}, pathPrepend)
	}
	return runCombined(ctx, t, workspace, tc, agentBin, tc.RunCommand(prompt), pathPrepend)
}

func runCombined(ctx context.Context, t *testing.T, dir string, tc agentCase, name string, args []string, pathPrepend string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = e2eAgentEnv(t, dir, tc.Credentials.EnvVars, agentExtraEnv(tc), pathPrepend)
	combined, err := cmd.CombinedOutput()
	output := string(combined)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("agent command timed out: %s %s\n%s", name, strings.Join(args, " "), output)
	}
	if err != nil {
		t.Fatalf("agent command failed: %s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return output
}

func agentExtraEnv(tc agentCase) map[string]string {
	if tc.AgentEnv == nil {
		return nil
	}
	return tc.AgentEnv()
}

func detectionPrompt(prompter string) string {
	command := shellQuote(prompter) + " --detect-only"
	return "Run this exact shell command from the current working directory and report its stdout: " + command + ". Do not explain; include the detector output string."
}

func overriddenBin(t *testing.T, key string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv("DISCOBOX_PROMPTER_E2E_" + key + "_BIN"))
	if value == "" {
		return ""
	}
	if !fileExists(value) {
		t.Fatalf("configured binary does not exist: DISCOBOX_PROMPTER_E2E_%s_BIN=%s", key, value)
	}
	return value
}

func lookPath(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s not found on PATH and %s=1", name, envSkipInstall)
	}
	return path
}

func lookPathOptional(name string) (string, bool) {
	path, err := exec.LookPath(name)
	return path, err == nil
}

func mergedEnv(extra map[string]string) []string {
	env := os.Environ()
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	return env
}

func e2eAgentEnv(t *testing.T, workspace string, credentialEnv []string, extra map[string]string, pathPrepend string) []string {
	t.Helper()

	home := filepath.Join(workspace, "home")
	xdgCache := filepath.Join(home, ".cache")
	xdgConfig := filepath.Join(home, ".config")
	xdgData := filepath.Join(home, ".local", "share")
	for _, dir := range []string{home, xdgCache, xdgConfig, xdgData} {
		mkdirAll(t, dir)
	}

	pathValue := os.Getenv("PATH")
	if pathPrepend != "" {
		pathValue = pathPrepend + string(os.PathListSeparator) + pathValue
	}

	base := map[string]string{
		"HOME":            home,
		"PATH":            pathValue,
		"SHELL":           "/bin/sh",
		"TERM":            "dumb",
		"TMPDIR":          os.TempDir(),
		"XDG_CACHE_HOME":  xdgCache,
		"XDG_CONFIG_HOME": xdgConfig,
		"XDG_DATA_HOME":   xdgData,
	}
	copyEnvIfSet(base, "LANG")
	copyEnvIfSet(base, "LC_ALL")
	copyEnvIfSet(base, "SSL_CERT_FILE")
	copyEnvIfSet(base, "SSL_CERT_DIR")
	copyEnvIfSet(base, "REQUESTS_CA_BUNDLE")
	copyEnvIfSet(base, "CURL_CA_BUNDLE")
	for _, envVar := range credentialEnv {
		copyEnvIfSet(base, envVar)
	}
	for key, value := range extra {
		base[key] = value
	}

	env := make([]string, 0, len(base))
	for _, key := range sortedKeys(base) {
		env = append(env, key+"="+base[key])
	}
	return env
}

func copyEnvIfSet(env map[string]string, key string) {
	if value, ok := os.LookupEnv(key); ok {
		env[key] = value
	}
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create directory %s: %v", dir, err)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func safePathName(value string) string {
	value = strings.TrimPrefix(value, "@")
	value = strings.ReplaceAll(value, "/", "__")
	value = strings.ReplaceAll(value, "\\", "__")
	return value
}

func discobotOpenAIModel() string {
	if model := strings.TrimSpace(os.Getenv("DISCOBOX_PROMPTER_E2E_DISCOBOT_OPENAI_MODEL")); model != "" {
		return model
	}
	return "openai/gpt-5.5"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func TestMain(m *testing.M) {
	if os.Getenv(envAgents) == "" {
		os.Exit(m.Run())
	}
	fmt.Fprintf(os.Stderr, "running prompter e2e tests with %s=%s\n", envAgents, os.Getenv(envAgents))
	os.Exit(m.Run())
}
