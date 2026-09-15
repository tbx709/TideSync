// Package service installs TideSync as a background service: a systemd unit on
// Linux (including Raspberry Pi OS) or a scheduled task / Windows service on
// Windows. The same configuration file is used in every case.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Role is what the installed service does.
type Role string

// Roles.
const (
	RoleClient Role = "client" // pulls from an agent, the common case
	RoleServer Role = "server" // publishes a directory (the agent)
)

// Options describes the service to install or query.
type Options struct {
	Role     Role
	Binary   string        // absolute path to the tidesync executable
	Config   string        // absolute path to the configuration file
	Interval time.Duration // scheduling interval (used by Task Scheduler)
	Name     string        // unit / task / service name
	User     string        // Linux only: run as this user (default: root)
	Mode     string        // auto | systemd | systemd-user | task | scm
	ExtraEnv []string      // extra environment variables (KEY=VALUE)
}

// NameOrDefault returns the configured name or the platform default.
func (o Options) NameOrDefault() string {
	if strings.TrimSpace(o.Name) != "" {
		return o.Name
	}
	if o.Role == RoleServer {
		return "tidesync-agent"
	}
	return "tidesync"
}

// DisplayName is the human readable service description.
func (o Options) DisplayName() string {
	if o.Role == RoleServer {
		return "TideSync agent (publishes a directory)"
	}
	return "TideSync synchroniser"
}

// ExecArgs returns the arguments the service passes to the binary.
func (o Options) ExecArgs() []string {
	args := []string{"sync", "-config", o.Config}
	if o.Role == RoleServer {
		args = []string{"serve", "-config", o.Config}
	}
	return args
}

// OneShotArgs returns the arguments used by schedulers that expect the process
// to finish (Windows Task Scheduler, cron).
func (o Options) OneShotArgs() []string {
	if o.Role == RoleServer {
		// The agent is a long running process even when started by a scheduler.
		return []string{"serve", "-config", o.Config}
	}
	return []string{"sync", "-config", o.Config, "-once"}
}

// IntervalOrDefault returns the scheduling interval, defaulting to 5 minutes.
func (o Options) IntervalOrDefault() time.Duration {
	if o.Interval <= 0 {
		return 5 * time.Minute
	}
	return o.Interval
}

// StatusInfo is the result of a status query.
type StatusInfo struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Manager   string `json:"manager"`
	Unit      string `json:"unit"`
	Detail    string `json:"detail"`
}

// Validate checks the options that every platform needs.
func (o *Options) Validate() error {
	if o.Role == "" {
		o.Role = RoleClient
	}
	if o.Role != RoleClient && o.Role != RoleServer {
		return fmt.Errorf("unknown role %q (want client or server)", o.Role)
	}
	if o.Binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot determine the path of this executable: %w", err)
		}
		o.Binary = exe
	}
	abs, err := filepath.Abs(o.Binary)
	if err != nil {
		return err
	}
	o.Binary = abs
	if o.Config == "" {
		return fmt.Errorf("-config is required for service installation")
	}
	cfgAbs, err := filepath.Abs(o.Config)
	if err != nil {
		return err
	}
	o.Config = cfgAbs
	if _, err := os.Stat(o.Config); err != nil {
		return fmt.Errorf("configuration file %s is not readable: %w", o.Config, err)
	}
	if _, err := os.Stat(o.Binary); err != nil {
		return fmt.Errorf("executable %s is not readable: %w", o.Binary, err)
	}
	return nil
}

// runCommand executes a helper program and returns its combined output.
func runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
		}
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}

// lookPath finds a helper program.
func lookPath(name string) (string, bool) {
	p, err := exec.LookPath(name)
	return p, err == nil
}

// QuoteCommandLine renders an argv for display in documentation and logs.
func QuoteCommandLine(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		if strings.ContainsAny(a, " \t\"") {
			parts = append(parts, `"`+strings.ReplaceAll(a, `"`, `\"`)+`"`)
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// HostDescription is used in generated unit descriptions.
func HostDescription() string {
	hn, err := os.Hostname()
	if err != nil {
		hn = "unknown"
	}
	return fmt.Sprintf("%s/%s on %s", runtime.GOOS, runtime.GOARCH, hn)
}
