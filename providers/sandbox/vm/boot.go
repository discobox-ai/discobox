package vm

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	workerbootstrap "github.com/obot-platform/discobox/workerbootstrap"
)

// InstanceSpec is the driver-neutral VM launch request.
type InstanceSpec struct {
	Ref       sandbox.SandboxRef
	Name      string
	Image     string
	Resources sandbox.ResourceConfig
	Boot      BootConfig
	Metadata  map[string]string
}

// Instance is the driver-neutral VM runtime state.
type Instance struct {
	ID        string
	Name      string
	Image     string
	Status    sandbox.Status
	AgentURL  string
	AgentHost string
	Metadata  map[string]string
	CreatedAt time.Time
	StartedAt *time.Time
	StoppedAt *time.Time
	Error     string
}

// BootInput is the Disco-specific data needed to render VM boot metadata.
type BootInput struct {
	Ref             sandbox.SandboxRef
	Options         sandbox.CreateOptions
	WorkerBootstrap workerbootstrap.Bootstrap
	ControlPlaneURL string
	AgentPort       int
}

// BootConfig contains boot metadata in common VM delivery forms.
type BootConfig struct {
	Env               map[string]string
	KernelCommandLine []string
	CloudInitUserData string
	CloudInitMetaData string
}

// BuildBootConfig renders worker registration settings as environment,
// kernel-command-line, and cloud-init metadata. Drivers should pass the subset
// their platform supports.
func BuildBootConfig(input BootInput) BootConfig {
	env := map[string]string{
		workerbootstrap.EnvControlPlaneURL: input.ControlPlaneURL,
		workerbootstrap.EnvProjectID:       firstNonEmpty(input.WorkerBootstrap.ProjectID, input.Ref.ProjectID),
		workerbootstrap.EnvSandboxID:       firstNonEmpty(input.WorkerBootstrap.SandboxID, input.Ref.SandboxID),
		workerbootstrap.EnvWorkerID:        input.WorkerBootstrap.WorkerID,
		workerbootstrap.EnvBootstrapToken:  input.WorkerBootstrap.Token,
	}
	if input.AgentPort > 0 {
		env[workerbootstrap.EnvAgentPort] = strconv.Itoa(input.AgentPort)
	}
	for key, value := range input.Options.Env {
		if _, reserved := env[key]; !reserved {
			env[key] = value
		}
	}
	removeEmpty(env)
	return BootConfig{
		Env:               env,
		KernelCommandLine: KernelCommandLine(env),
		CloudInitUserData: CloudInitUserData(env),
		CloudInitMetaData: CloudInitMetaData(input),
	}
}

// KernelCommandLine renders boot env as discobox.key=value kernel args.
func KernelCommandLine(env map[string]string) []string {
	keys := sortedKeys(env)
	args := make([]string, 0, len(keys))
	for _, key := range keys {
		args = append(args, fmt.Sprintf("discobox.%s=%s", strings.ToLower(key), quoteKernelArg(env[key])))
	}
	return args
}

// CloudInitUserData renders a cloud-init document that writes worker bootstrap
// environment for the in-guest worker agent.
func CloudInitUserData(env map[string]string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("write_files:\n")
	b.WriteString("  - path: /etc/discobox/worker.env\n")
	b.WriteString("    permissions: '0600'\n")
	b.WriteString("    content: |\n")
	for _, key := range sortedKeys(env) {
		b.WriteString("      ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(env[key]))
		b.WriteString("\n")
	}
	b.WriteString("runcmd:\n")
	b.WriteString("  - [ systemctl, restart, discobox-worker-agent ]\n")
	return b.String()
}

// CloudInitMetaData renders stable instance metadata.
func CloudInitMetaData(input BootInput) string {
	workerID := input.WorkerBootstrap.WorkerID
	if workerID == "" {
		workerID = input.Ref.SandboxID
	}
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", workerID, instanceName(input.Ref))
}

type stateData struct {
	InstanceID string          `json:"instanceId"`
	Worker     WorkerBootstrap `json:"worker,omitempty"`
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func removeEmpty(values map[string]string) {
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			delete(values, key)
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func quoteKernelArg(value string) string {
	return strings.NewReplacer(" ", "\\x20", "\n", "", "\t", "").Replace(value)
}

func shellQuote(value string) string {
	return strconv.Quote(value)
}
