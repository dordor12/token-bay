//go:build e2e

package driver

import (
	"fmt"
	"os"
	"os/exec"
)

// Compose wraps `docker compose` invocations against a single compose
// file (tracker/test/e2e/compose.e2e.yaml, per Task 22). Project is
// optional — when empty, `-p` is omitted and docker compose derives the
// project name from the compose file's directory (its normal default).
type Compose struct {
	File    string
	Project string
}

// composeArgs is the pure arg-vector builder shared by every subcommand
// below. Extracted so driver_test.go can assert the exact argument list
// docker would receive without spawning a process.
func (c Compose) composeArgs(sub ...string) []string {
	args := []string{"compose", "-f", c.File}
	if c.Project != "" {
		args = append(args, "-p", c.Project)
	}
	return append(args, sub...)
}

// UpArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] up -d --build`.
func (c Compose) UpArgs() []string {
	return c.composeArgs("up", "-d", "--build")
}

// DownArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] down [-v]`.
func (c Compose) DownArgs(removeVolumes bool) []string {
	sub := []string{"down"}
	if removeVolumes {
		sub = append(sub, "-v")
	}
	return c.composeArgs(sub...)
}

// ExecArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] exec -T <service> <args...>`. The -T disables pseudo-tty
// allocation so output is plain and script-friendly.
func (c Compose) ExecArgs(service string, args ...string) []string {
	sub := append([]string{"exec", "-T", service}, args...)
	return c.composeArgs(sub...)
}

// LogsArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] logs <service>`.
func (c Compose) LogsArgs(service string) []string {
	return c.composeArgs("logs", service)
}

// PsArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] ps`.
func (c Compose) PsArgs() []string {
	return c.composeArgs("ps")
}

// Up runs `docker compose ... up -d --build`, streaming docker's own
// stdout/stderr so a hung bring-up is visible in CI logs.
func (c Compose) Up() error {
	//nolint:gosec // G204: args come from the fixed compose file/project
	// configured by the test harness (not external/attacker input) — the
	// whole point of this package is to shell out to docker.
	cmd := exec.Command("docker", c.UpArgs()...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("driver: compose up: %w", err)
	}
	return nil
}

// Down runs `docker compose ... down`, optionally removing named
// volumes (removeVolumes=true tears down tracker-a-data/tracker-b-data
// so the next Up starts from a clean ledger).
func (c Compose) Down(removeVolumes bool) error {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.DownArgs(removeVolumes)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("driver: compose down: %w", err)
	}
	return nil
}

// Exec runs `docker compose ... exec -T <service> <args...>` and returns
// combined stdout+stderr. Non-zero exit is returned as an error wrapping
// the captured output for debuggability.
func (c Compose) Exec(service string, args ...string) (string, error) {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.ExecArgs(service, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("driver: compose exec %s %v: %w: %s", service, args, err, out)
	}
	return string(out), nil
}

// Logs runs `docker compose ... logs <service>` and returns combined
// stdout+stderr.
func (c Compose) Logs(service string) (string, error) {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.LogsArgs(service)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("driver: compose logs %s: %w: %s", service, err, out)
	}
	return string(out), nil
}

// Ps runs `docker compose ... ps` and returns combined stdout+stderr.
func (c Compose) Ps() (string, error) {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.PsArgs()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("driver: compose ps: %w: %s", err, out)
	}
	return string(out), nil
}
