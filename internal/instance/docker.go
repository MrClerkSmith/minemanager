// Docker runtime: run the server inside a container while keeping the same
// console, command and crash-detection machinery as the native runtime.
package instance

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// containerName is the docker container name for this instance.
func (in *Instance) containerName() string {
	return "mc-" + in.cfg.ID
}

// imageName picks an official Temurin image able to run the game version.
func (in *Instance) imageName() string {
	return fmt.Sprintf("eclipse-temurin:%d-jre", javaMajor(in.cfg.Version))
}

// rconPort resolves the configured (or derived) RCON port.
func (in *Instance) rconPort() int {
	if in.cfg.RCONPort != 0 {
		return in.cfg.RCONPort
	}
	return in.cfg.Port + 10
}

// launchDocker starts the server inside a container attached to this instance.
func (in *Instance) launchDocker(ctx context.Context) error {
	name := in.containerName()
	// Remove a container left over from a previous run.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()

	if err := in.ensureImage(ctx); err != nil {
		in.setState(StateError)
		return err
	}

	args := []string{
		"run", "-d", "-i",
		"--name", name,
		"--restart=no",
		"-v", in.dir + ":/server",
		"-w", "/server",
		"-p", fmt.Sprintf("%d:%d", in.cfg.Port, in.cfg.Port),
		"-p", fmt.Sprintf("%d:%d", in.rconPort(), in.rconPort()),
		in.imageName(),
	}
	args = append(args, in.buildArgs()...)

	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		in.setState(StateError)
		return fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(string(out)))
	}

	in.mu.Lock()
	in.done = make(chan struct{})
	in.startedAt = time.Now()
	in.stopSignaled = false
	in.crashFlag = false
	in.mu.Unlock()

	in.recordEvent("start", "started container %s image %s", name, in.imageName())
	go in.pumpDockerLogs(ctx)
	go func() {
		code := in.dockerWait(ctx)
		close(in.done)
		in.supervise(code)
	}()
	return nil
}

// ensureImage pulls the JVM image when it is not present locally.
func (in *Instance) ensureImage(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", in.imageName()).Run(); err == nil {
		return nil
	}
	in.log.Writef("[Manager] pulling docker image %s", in.imageName())
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "pull", in.imageName())
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker pull %s: %w: %s", in.imageName(), err, strings.TrimSpace(buf.String()))
	}
	return nil
}

// pumpDockerLogs streams the container stdout/stderr into the console log.
func (in *Instance) pumpDockerLogs(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", in.containerName())
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.log.Writef("[Manager] docker logs pipe failed: %v", err)
		return
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = out.Close()
		in.log.Writef("[Manager] docker logs pipe failed: %v", err)
		return
	}
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		_ = errPipe.Close()
		in.log.Writef("[Manager] docker logs failed: %v", err)
		return
	}
	go in.pump(out, "docker stdout")
	in.pump(errPipe, "docker stderr")
	_ = cmd.Wait()
}

// dockerWait blocks until the container exits and returns its status code.
func (in *Instance) dockerWait(ctx context.Context) int {
	cmd := exec.CommandContext(ctx, "docker", "wait", in.containerName())
	out, err := cmd.Output()
	if err != nil {
		return -1
	}
	var code int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &code)
	return code
}

// stopDocker stops the container, first trying a graceful console shutdown.
func (in *Instance) stopDocker(ctx context.Context) error {
	in.mu.Lock()
	done := in.done
	in.stopSignaled = true
	in.mu.Unlock()
	if done == nil {
		return ErrNotRunning
	}
	in.setState(StateStopping)
	in.log.Write("[Manager] stopping container")
	_ = in.dockerSendCommand("stop")

	select {
	case <-done:
		return nil
	case <-time.After(60 * time.Second):
		in.log.Write("[Manager] graceful stop timed out, removing container")
		return in.dockerKill()
	}
}

// dockerKill removes the container forcefully.
func (in *Instance) dockerKill() error {
	in.mu.Lock()
	in.stopSignaled = true
	in.mu.Unlock()
	return exec.Command("docker", "rm", "-f", in.containerName()).Run()
}

// dockerSendCommand types a line into the container's server process by
// writing to PID 1 stdin, keeping RCON as an alternative.
func (in *Instance) dockerSendCommand(line string) error {
	if !in.IsRunning() {
		return ErrNotRunning
	}
	var buf bytes.Buffer
	cmd := exec.Command("docker", "exec", "-i", in.containerName(),
		"sh", "-c", `printf '%s\n' "$1" > /proc/1/fd/0`, "_", line)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		// Fall back to RCON for servers that have it enabled.
		if _, rerr := in.exec(line); rerr == nil {
			return nil
		}
		return fmt.Errorf("docker exec: %w: %s", err, strings.TrimSpace(buf.String()))
	}
	return nil
}
