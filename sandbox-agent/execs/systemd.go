package execs

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type SystemdRunner struct{}

func (SystemdRunner) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return StartResult{}, fmt.Errorf("exec command is required")
	}
	exe, err := os.Executable()
	if err != nil {
		return StartResult{}, err
	}
	commandJSON, err := json.Marshal(req.Command)
	if err != nil {
		return StartResult{}, err
	}
	envJSON, err := json.Marshal(req.Env)
	if err != nil {
		return StartResult{}, err
	}
	args := []string{
		"--unit=" + req.Unit,
		"--collect",
		"--property=KillMode=control-group",
		"--property=WorkingDirectory=" + req.Workdir,
	}
	if req.UID != nil {
		args = append(args, "--uid="+strconv.FormatInt(*req.UID, 10))
	}
	if req.GID != nil {
		args = append(args, "--gid="+strconv.FormatInt(*req.GID, 10))
	}
	for key, value := range req.Env {
		if strings.TrimSpace(key) != "" {
			args = append(args, "--setenv="+key+"="+value)
		}
	}
	args = append(args, "--")
	args = append(args,
		exe,
		"exec-shim",
		"--exec-id", req.ID,
		"--unit", req.Unit,
		"--workdir", req.Workdir,
		"--socket", req.SocketPath,
		"--runtime", req.RuntimePath,
		"--logs", req.LogDir,
		"--rows", strconv.Itoa(int(req.Rows)),
		"--cols", strconv.Itoa(int(req.Cols)),
		"--command", base64.StdEncoding.EncodeToString(commandJSON),
		"--env", base64.StdEncoding.EncodeToString(envJSON),
	)
	if req.TTY {
		args = append(args, "--tty")
	}
	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return StartResult{}, fmt.Errorf("systemd-run %s: %w: %s", req.Unit, err, strings.TrimSpace(string(output)))
	}
	return StartResult{Unit: req.Unit}, nil
}

func (SystemdRunner) Status(ctx context.Context, unit string) (UnitStatus, error) {
	if strings.TrimSpace(unit) == "" {
		return UnitStatus{}, fmt.Errorf("unit is required")
	}
	props := []string{
		"Id",
		"ActiveState",
		"SubState",
		"MainPID",
		"ExecMainPID",
		"ExecMainStatus",
		"Result",
		"ActiveEnterTimestamp",
		"InactiveEnterTimestamp",
	}
	args := []string{"show", unit, "--no-pager"}
	for _, prop := range props {
		args = append(args, "--property="+prop)
	}
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	output, err := cmd.Output()
	if err != nil {
		return UnitStatus{}, err
	}
	return unitStatusFromProperties(parseProperties(string(output))), nil
}

func (SystemdRunner) List(ctx context.Context) ([]UnitStatus, error) {
	cmd := exec.CommandContext(ctx, "systemctl", "list-units", "--all", "--no-legend", "--plain", "discobox-exec-*")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var out []UnitStatus
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		status, err := SystemdRunner{}.Status(ctx, fields[0])
		if err != nil {
			continue
		}
		out = append(out, status)
	}
	return out, scanner.Err()
}

func parseProperties(output string) map[string]string {
	props := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if ok {
			props[key] = value
		}
	}
	return props
}

func unitStatusFromProperties(props map[string]string) UnitStatus {
	status := UnitStatus{Unit: props["Id"]}
	active := props["ActiveState"]
	sub := props["SubState"]
	switch active {
	case "active", "activating", "reloading":
		status.Active = true
		status.Status = StatusRunning
	case "failed":
		status.Status = StatusFailed
	case "inactive":
		status.Status = StatusExited
	default:
		status.Status = StatusLost
	}
	if sub == "dead" && status.Status == StatusRunning {
		status.Status = StatusExited
	}
	pid := parseInt64(props["MainPID"])
	if pid == 0 {
		pid = parseInt64(props["ExecMainPID"])
	}
	status.PID = pid
	if code := parseInt64(props["ExecMainStatus"]); code != 0 || status.Status == StatusFailed {
		status.ExitCode = &code
	}
	if result := strings.TrimSpace(props["Result"]); result != "" && result != "success" {
		status.Error = result
	}
	if started := parseSystemdTime(props["ActiveEnterTimestamp"]); started != nil {
		status.StartedAt = started
	}
	if exited := parseSystemdTime(props["InactiveEnterTimestamp"]); exited != nil {
		status.ExitedAt = exited
	}
	return status
}

func parseInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed
}

func parseSystemdTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" || value == "n/a" {
		return nil
	}
	layouts := []string{
		"Mon 2006-01-02 15:04:05 MST",
		"Mon 2006-01-02 15:04:05.000000 MST",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}
