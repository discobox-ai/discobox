package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/obot-platform/discobox/harness"
)

const (
	ManagedConfigDir  = "/etc/opencode"
	ManagedConfigPath = "/etc/opencode/opencode.json"
	PluginPath        = "/etc/opencode/plugins/discobox-hook-publish.js"
)

type Driver struct{}

func (Driver) ID() string { return "opencode" }

func (Driver) Install(_ context.Context, req harness.InstallRequest) error {
	configDir := harness.ManagedPath(req.ManagedRoot, ManagedConfigDir)
	harness.SetEnv(req.Env, "OPENCODE_CONFIG_DIR", configDir)
	if err := harness.MergeJSONFile(harness.ManagedPath(req.ManagedRoot, ManagedConfigPath), func(config map[string]any) {
		if _, ok := config["$schema"]; !ok {
			config["$schema"] = "https://opencode.ai/config.json"
		}
	}); err != nil {
		return err
	}
	pluginPath := harness.ManagedPath(req.ManagedRoot, PluginPath)
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(pluginPath, []byte(pluginSource(harness.PublisherCommand(req))), 0o600)
}

func pluginSource(publisher string) string {
	argv := jsArray(strings.Fields(publisher))
	return `import { spawn } from "node:child_process"

const baseCommand = ` + argv + `

async function publish(event, payload) {
  const args = [...baseCommand.slice(1), "--provider", "opencode", "--event", event]
  const child = spawn(baseCommand[0], args, {
    stdio: ["pipe", "ignore", "ignore"],
  })
  child.stdin.end(JSON.stringify(payload ?? {}))
  await new Promise((resolve) => child.on("close", resolve))
}

export const DiscoboxHookPublish = async () => {
  return {
    event: async ({ event }) => {
      await publish(event.type || "event", event)
    },
    "tool.execute.before": async (input, output) => {
      await publish("tool.execute.before", { input, output })
    },
    "tool.execute.after": async (input, output) => {
      await publish("tool.execute.after", { input, output })
    },
    "shell.env": async (input, output) => {
      await publish("shell.env", { input, output })
    },
  }
}
`
}

func jsArray(values []string) string {
	if len(values) == 0 {
		values = []string{"discobox-hook-publish"}
	}
	out := "["
	for i, value := range values {
		if i > 0 {
			out += ", "
		}
		out += `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return out + "]"
}
