//go:build e2e || perf

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
// ExtraFiles, when non-empty, are layered over File as additional -f
// flags (docker compose override semantics) — the coverage run
// (test/e2e/run-cover.sh) uses this so scenario-driven subcommands
// (notably Run, which creates a fresh container from the file
// definitions) resolve to the same overridden topology
// (compose.cover.yaml) the stack was started with.
type Compose struct {
	File       string
	ExtraFiles []string
	Project    string
}

// composeArgs is the pure arg-vector builder shared by every subcommand
// below. Extracted so driver_test.go can assert the exact argument list
// docker would receive without spawning a process.
func (c Compose) composeArgs(sub ...string) []string {
	args := []string{"compose", "-f", c.File}
	for _, f := range c.ExtraFiles {
		args = append(args, "-f", f)
	}
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

// PsAllArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] ps -a`, which (unlike PsArgs) includes stopped/exited
// containers — the ledger-integrity scenarios (test/e2e/ledger_test.go)
// need to see a crashed tracker-a, not just running ones.
func (c Compose) PsAllArgs() []string {
	return c.composeArgs("ps", "-a")
}

// RestartArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] restart <service>`. Restarts the existing container in
// place (no recreation), so its log stream is cumulative across the
// restart.
func (c Compose) RestartArgs(service string) []string {
	return c.composeArgs("restart", service)
}

// StopArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] stop <service>`. Sends SIGTERM (then SIGKILL after the
// compose default timeout) and leaves the container in the "exited"
// state rather than removing it.
func (c Compose) StopArgs(service string) []string {
	return c.composeArgs("stop", service)
}

// StartArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] start <service>`. Starts an existing (stopped) service
// container in place — the counterpart to StopArgs.
func (c Compose) StartArgs(service string) []string {
	return c.composeArgs("start", service)
}

// KillArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] kill -s <signal> <service>`.
func (c Compose) KillArgs(service, signal string) []string {
	return c.composeArgs("kill", "-s", signal, service)
}

// RunArgs returns the arg vector for `docker compose -f <file> [-p
// <project>] run --rm -T [--entrypoint <entrypoint>] <service>
// [args...]`. Unlike Exec, Run does not require the service's container
// to already be running — it starts a new, ephemeral container attached
// to the same named volumes, and (because --service-ports is not
// passed) does not publish the service's ports, so it never conflicts
// with a live sibling container of the same service. entrypoint="" omits
// the --entrypoint override and uses the image's configured entrypoint
// (e.g. token-bay-tracker itself) plus the service's configured command.
func (c Compose) RunArgs(service, entrypoint string, args ...string) []string {
	sub := []string{"run", "--rm", "-T"}
	if entrypoint != "" {
		sub = append(sub, "--entrypoint", entrypoint)
	}
	sub = append(sub, service)
	sub = append(sub, args...)
	return c.composeArgs(sub...)
}

// PsAll runs `docker compose ... ps -a` and returns combined
// stdout+stderr.
func (c Compose) PsAll() (string, error) {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.PsAllArgs()...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("driver: compose ps -a: %w: %s", err, out)
	}
	return string(out), nil
}

// Restart runs `docker compose ... restart <service>`, streaming
// docker's own stdout/stderr (mirrors Up/Down — a hung restart should be
// visible in CI logs, not silently swallowed).
func (c Compose) Restart(service string) error {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.RestartArgs(service)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("driver: compose restart %s: %w", service, err)
	}
	return nil
}

// Stop runs `docker compose ... stop <service>`.
func (c Compose) Stop(service string) error {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.StopArgs(service)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("driver: compose stop %s: %w", service, err)
	}
	return nil
}

// Start runs `docker compose ... start <service>`.
func (c Compose) Start(service string) error {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.StartArgs(service)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("driver: compose start %s: %w", service, err)
	}
	return nil
}

// Kill runs `docker compose ... kill -s <signal> <service>`. Kill only
// delivers the signal — it returns as soon as the signal is sent, before
// the container has necessarily exited, so callers must poll (e.g. via
// PsAll) for the resulting container state.
func (c Compose) Kill(service, signal string) error {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.KillArgs(service, signal)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("driver: compose kill -s %s %s: %w", signal, service, err)
	}
	return nil
}

// Run runs `docker compose ... run --rm -T [--entrypoint <entrypoint>]
// <service> <args...>` and returns combined stdout+stderr. Non-zero exit
// is returned as an error wrapping the captured output, matching Exec's
// contract.
func (c Compose) Run(service, entrypoint string, args ...string) (string, error) {
	//nolint:gosec // G204: see Up.
	cmd := exec.Command("docker", c.RunArgs(service, entrypoint, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("driver: compose run %s %v: %w: %s", service, args, err, out)
	}
	return string(out), nil
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
