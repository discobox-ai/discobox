package execs

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
)

// shimArgv is the command line a unit runs: this same binary, as the exec
// shim, carrying the exec's inputs as base64 JSON. The shim keeps the unit's
// own identity — no unit manager drops privileges for it — because it drops to
// the run user itself after it has set up the PTY and the socket.
//
// ParseShimArgs is its inverse, and the two live together so the contract
// between whatever starts a shim and the shim itself is one file.
func shimArgv(exe string, req StartRequest) ([]string, error) {
	commandJSON, err := json.Marshal(req.Command)
	if err != nil {
		return nil, err
	}
	startupCommandJSON, err := json.Marshal(req.StartupCommand)
	if err != nil {
		return nil, err
	}
	envJSON, err := json.Marshal(req.Env)
	if err != nil {
		return nil, err
	}
	userJSON, err := json.Marshal(req.User)
	if err != nil {
		return nil, err
	}
	metadataJSON, err := json.Marshal(req.Metadata)
	if err != nil {
		return nil, err
	}
	argv := []string{
		exe,
		"exec-shim",
		"--exec-id", req.ID,
		"--unit", req.Unit,
		"--workdir", req.Workdir,
		"--socket", req.SocketPath,
		"--runtime", req.RuntimePath,
		"--database", req.DatabasePath,
		"--rows", strconv.Itoa(int(req.Rows)),
		"--cols", strconv.Itoa(int(req.Cols)),
		"--command", base64.StdEncoding.EncodeToString(commandJSON),
		"--startup-command", base64.StdEncoding.EncodeToString(startupCommandJSON),
		"--env", base64.StdEncoding.EncodeToString(envJSON),
		"--user", base64.StdEncoding.EncodeToString(userJSON),
		"--metadata", base64.StdEncoding.EncodeToString(metadataJSON),
	}
	if req.TTY {
		argv = append(argv, "--tty")
	}
	return argv, nil
}

// ShimArgs is an exec-shim command line, read back.
type ShimArgs struct {
	Config ShimConfig
	// DatabasePath is the sqlite file the shim opens its own connection to for
	// the exec's transcript; opening it is the caller's, which owns the store.
	DatabasePath string
	// Lifetime is the descriptor a Supervisor passed the shim to hold for as
	// long as it runs, or nil under a unit manager that has its own way to tell
	// (see Supervisor). The shim must call HoldLifetime on it before it starts
	// anything.
	Lifetime *uintptr
}

// ParseShimArgs reads the arguments that follow "exec-shim", as shimArgv
// wrote them.
func ParseShimArgs(args []string) (ShimArgs, error) {
	var (
		out                                                                        ShimArgs
		cfg                                                                        = &out.Config
		commandBase64, startupCommandBase64, envBase64, userBase64, metadataBase64 string
		lifetime                                                                   string
		rows, cols                                                                 int
	)
	flags := flag.NewFlagSet("discobox-sandbox-agent exec-shim", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.ExecID, "exec-id", "", "sandbox exec id")
	flags.StringVar(&cfg.Unit, "unit", "", "unit name")
	flags.StringVar(&cfg.Workdir, "workdir", "", "exec working directory")
	flags.StringVar(&cfg.SocketPath, "socket", "", "exec shim unix socket path")
	flags.StringVar(&cfg.RuntimePath, "runtime", "", "exec runtime status path")
	flags.StringVar(&out.DatabasePath, "database", "", "sandbox-agent sqlite database path")
	flags.IntVar(&rows, "rows", 0, "initial PTY rows")
	flags.IntVar(&cols, "cols", 0, "initial PTY cols")
	flags.BoolVar(&cfg.TTY, "tty", false, "allocate a PTY")
	flags.StringVar(&commandBase64, "command", "", "base64 encoded JSON command argv")
	flags.StringVar(&startupCommandBase64, "startup-command", "", "base64 encoded JSON startup command argv, typed into the shell after it starts")
	flags.StringVar(&envBase64, "env", "", "base64 encoded JSON environment")
	flags.StringVar(&userBase64, "user", "", "base64 encoded JSON exec user")
	flags.StringVar(&metadataBase64, "metadata", "", "base64 encoded JSON exec metadata")
	flags.StringVar(&lifetime, "lifetime", "", "descriptor held for the shim's lifetime (Supervisor)")
	if err := flags.Parse(args); err != nil {
		return ShimArgs{}, err
	}
	if commandBase64 == "" {
		return ShimArgs{}, errors.New("exec shim command is required")
	}
	for _, field := range []struct {
		name    string
		encoded string
		into    any
	}{
		{"command", commandBase64, &cfg.Command},
		{"startup command", startupCommandBase64, &cfg.StartupCommand},
		{"env", envBase64, &cfg.Env},
		{"user", userBase64, &cfg.User},
		{"metadata", metadataBase64, &cfg.Metadata},
	} {
		if field.encoded == "" {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(field.encoded)
		if err != nil {
			return ShimArgs{}, fmt.Errorf("decode exec shim %s: %w", field.name, err)
		}
		if err := json.Unmarshal(decoded, field.into); err != nil {
			return ShimArgs{}, fmt.Errorf("parse exec shim %s: %w", field.name, err)
		}
	}
	if lifetime != "" {
		fd, err := strconv.ParseUint(lifetime, 10, 64)
		if err != nil {
			return ShimArgs{}, fmt.Errorf("parse exec shim lifetime descriptor: %w", err)
		}
		value := uintptr(fd)
		out.Lifetime = &value
	}
	cfg.Rows = uint16Dimension(rows)
	cfg.Cols = uint16Dimension(cols)
	return out, nil
}

func uint16Dimension(value int) uint16 {
	if value <= 0 {
		return 0
	}
	if value > int(^uint16(0)) {
		return ^uint16(0)
	}
	return uint16(value)
}
