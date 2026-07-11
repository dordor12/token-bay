//go:build e2e

package driver

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	tccompose "github.com/testcontainers/testcontainers-go/modules/compose"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Stack owns the e2e topology via testcontainers-go's compose module,
// replacing the docker-compose shell-out (driver.Compose). It brings up
// compose.e2e.yaml programmatically and exposes the per-service operations the
// scenarios use by resolving each service to its *testcontainers.DockerContainer.
//
// The compose file (test/e2e/compose.e2e.yaml) remains the single topology
// source of truth — Stack drives that exact file, so the fixed host port
// mappings the scenario clients rely on are unchanged. compose.e2e.yaml also
// stays usable for manual `docker compose up` debugging.
//
// OrbStack note: testcontainers' Ryuk reaper is flaky on OrbStack; the e2e
// make target sets TESTCONTAINERS_RYUK_DISABLED=true and Stack.Down handles
// teardown explicitly.
type Stack struct {
	stack   tccompose.ComposeStack
	files   []string
	project string
	env     map[string]string
}

// NewStack builds a Stack over the given compose files. project is the
// compose project identifier; env is layered onto every service.
func NewStack(files []string, project string, env map[string]string) (*Stack, error) {
	cs, err := tccompose.NewDockerComposeWith(
		tccompose.WithStackFiles(files...),
		tccompose.StackIdentifier(project),
	)
	if err != nil {
		return nil, fmt.Errorf("driver: new compose stack: %w", err)
	}
	return &Stack{stack: cs, files: files, project: project, env: env}, nil
}

// Up brings the topology up and blocks until the trackers pass their compose
// healthchecks. Actors have no healthcheck; TestMain polls their readiness.
func (s *Stack) Up(ctx context.Context) error {
	cs := s.stack.WithEnv(s.env).
		WaitForService("tracker-a", wait.ForHealthCheck().WithStartupTimeout(120*time.Second)).
		WaitForService("tracker-b", wait.ForHealthCheck().WithStartupTimeout(120*time.Second))
	if err := cs.Up(ctx, tccompose.Wait(true)); err != nil {
		return fmt.Errorf("driver: compose up: %w", err)
	}
	return nil
}

// Down tears the stack down, removing containers, networks and volumes.
func (s *Stack) Down(ctx context.Context) error {
	return s.stack.Down(ctx,
		tccompose.RemoveOrphans(true),
		tccompose.RemoveVolumes(true),
		tccompose.RemoveImagesLocal,
	)
}

func (s *Stack) container(ctx context.Context, svc string) (*testcontainers.DockerContainer, error) {
	c, err := s.stack.ServiceContainer(ctx, svc)
	if err != nil {
		return nil, fmt.Errorf("driver: resolve service %q: %w", svc, err)
	}
	return c, nil
}

// Exec runs a command inside the service container and returns its combined
// (de-multiplexed) output — the drop-in for Compose.Exec used by sqlCount.
func (s *Stack) Exec(svc string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := s.container(ctx, svc)
	if err != nil {
		return "", err
	}
	code, r, err := c.Exec(ctx, args, tcexec.Multiplexed())
	if err != nil {
		return "", fmt.Errorf("driver: exec %q in %s: %w", strings.Join(args, " "), svc, err)
	}
	out, _ := io.ReadAll(r)
	if code != 0 {
		return string(out), fmt.Errorf("driver: exec %q in %s: exit %d", strings.Join(args, " "), svc, code)
	}
	return string(out), nil
}

// Logs returns the full stdout+stderr log of the service container.
func (s *Stack) Logs(svc string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := s.container(ctx, svc)
	if err != nil {
		return "", err
	}
	rc, err := c.Logs(ctx)
	if err != nil {
		return "", fmt.Errorf("driver: logs %s: %w", svc, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b), nil
}

// Stop gracefully stops the service container (SIGTERM, then SIGKILL after the
// grace period). The generous timeout lets a coverage-instrumented tracker
// flush before exiting.
func (s *Stack) Stop(svc string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := s.container(ctx, svc)
	if err != nil {
		return err
	}
	d := 75 * time.Second
	if err := c.Stop(ctx, &d); err != nil {
		return fmt.Errorf("driver: stop %s: %w", svc, err)
	}
	return nil
}

// Start (re)starts a previously-stopped service container.
func (s *Stack) Start(svc string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := s.container(ctx, svc)
	if err != nil {
		return err
	}
	if err := c.Start(ctx); err != nil {
		return fmt.Errorf("driver: start %s: %w", svc, err)
	}
	return nil
}

// Restart stops then starts the service container.
func (s *Stack) Restart(svc string) error {
	if err := s.Stop(svc); err != nil {
		return err
	}
	return s.Start(svc)
}

// Kill stops the service with a signal. Only SIGTERM is used by scenarios
// (graceful-shutdown / coverage-flush), which Stop delivers as the container
// stop signal; other signals fall back to the same graceful stop.
func (s *Stack) Kill(svc, _ string) error {
	return s.Stop(svc)
}

// PsAll returns `docker ps -a` scoped to this compose project, for debug
// dumps. Best-effort: a non-nil error is surfaced to the caller's t.Logf.
func (s *Stack) PsAll() (string, error) {
	//nolint:gosec // G204: fixed args + the process's own project label.
	cmd := exec.Command("docker", "ps", "-a",
		"--filter", "label=com.docker.compose.project="+s.project,
		"--format", "table {{.Names}}\t{{.Status}}\t{{.Image}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("driver: docker ps -a: %w", err)
	}
	return string(out), nil
}

// Run executes a one-off command in a fresh container built from the service's
// image, mounting that service's /data volume — the drop-in for Compose.Run
// used by the ledger corruption-tripwire cleanup, whose own tracker container
// may currently be exited (so Exec is not an option). Returns the container's
// combined logs.
func (s *Stack) Run(svc, entrypoint string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	image, vol, err := s.imageAndDataVolume(ctx, svc)
	if err != nil {
		return "", err
	}

	one, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      image,
			Entrypoint: []string{entrypoint},
			Cmd:        args,
			Mounts: testcontainers.ContainerMounts{
				{Source: testcontainers.GenericVolumeMountSource{Name: vol}, Target: "/data"},
			},
			WaitingFor: wait.ForExit().WithExitTimeout(45 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		return "", fmt.Errorf("driver: run one-off for %s: %w", svc, err)
	}
	defer func() { _ = one.Terminate(context.Background()) }()

	rc, lerr := one.Logs(ctx)
	if lerr != nil {
		return "", fmt.Errorf("driver: run logs for %s: %w", svc, lerr)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b), nil
}

// imageAndDataVolume inspects the service container to discover its image and
// the name of the volume bound at /data, so a one-off Run can reattach it.
func (s *Stack) imageAndDataVolume(ctx context.Context, svc string) (image, volume string, err error) {
	c, err := s.container(ctx, svc)
	if err != nil {
		return "", "", err
	}
	info, err := c.Inspect(ctx)
	if err != nil {
		return "", "", fmt.Errorf("driver: inspect %s: %w", svc, err)
	}
	image = info.Config.Image
	for _, m := range info.Mounts {
		if m.Destination == "/data" {
			volume = m.Name
			break
		}
	}
	if volume == "" {
		return "", "", fmt.Errorf("driver: %s has no /data volume mount", svc)
	}
	return image, volume, nil
}
