// Console and process management: native process launch, stdout/stderr
// pumping with crash + readiness detection, and console command input.
package instance

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Errors returned by lifecycle operations.
var (
	ErrNotRunning     = errors.New("server is not running")
	ErrAlreadyRunning = errors.New("server is already running")
	ErrBusy           = errors.New("server is busy, wait for the current operation to finish")
)

// mcTimestamp matches Minecraft's own [HH:MM:SS] line prefix in both the
// "[..] [thread/INFO]:" and the "[.. INFO]:" log formats.
var mcTimestamp = regexp.MustCompile(`^\[\d{2}:\d{2}:\d{2}\]?\s*`)

// consoleChunk caps one log line; Java stack traces can be extremely long.
const consoleChunk = 4000

// launchNative starts the java process with pipes attached to the manager.
func (in *Instance) launchNative(ctx context.Context) error {
	java, err := in.javaBinary()
	if err != nil {
		in.setState(StateError)
		return err
	}
	args := in.buildArgs()

	cmd := exec.Command(java, args...)
	cmd.Dir = in.dir
	cmd.Env = append(os.Environ(),
		"JAVA_TOOL_OPTIONS=", // do not inherit a global tool options file
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		in.setState(StateError)
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		in.setState(StateError)
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		in.setState(StateError)
		return err
	}
	if err := cmd.Start(); err != nil {
		in.setState(StateError)
		return fmt.Errorf("start java: %w", err)
	}

	in.mu.Lock()
	in.cmd = cmd
	in.stdin = stdin
	in.done = make(chan struct{})
	in.startedAt = time.Now()
	in.stopSignaled = false
	in.crashFlag = false
	in.mu.Unlock()

	in.recordEvent("start", "launched %s %s (pid %d)",
		java, strings.Join(args, " "), cmd.Process.Pid)

	go in.pump(stdout, "stdout")
	go in.pump(stderr, "stderr")
	go func() {
		_ = cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		close(in.done)
		in.supervise(code)
	}()
	return nil
}

// pump copies a process stream into the logger while analysing each line.
func (in *Instance) pump(r io.Reader, name string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 32<<20)
	for scanner.Scan() {
		in.handleConsoleLine(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		in.log.Writef("[Manager] %s stream ended with error: %v", name, err)
	}
}

// handleConsoleLine writes one server line and watches for crash / ready markers.
func (in *Instance) handleConsoleLine(text string) {
	// Minecraft prefixes its own lines with [HH:MM:SS]; the logger adds its own
	// timestamp, so drop the redundant one to avoid doubled prefixes.
	text = mcTimestamp.ReplaceAllString(text, "")
	for len(text) > consoleChunk {
		in.log.Write(text[:consoleChunk])
		text = text[consoleChunk:]
	}
	in.log.Write(text)

	in.mu.Lock()
	alreadyCrashed := in.crashFlag
	in.mu.Unlock()
	if !alreadyCrashed {
		for _, p := range fatalPatterns {
			if strings.Contains(text, p) {
				in.mu.Lock()
				in.crashFlag = true
				in.mu.Unlock()
				in.recordEvent("crash", "crash signature detected: %q", p)
				break
			}
		}
	}
	for _, p := range warnPatterns {
		if strings.Contains(text, p) {
			in.recordEvent("warning", "console warning: %q", p)
			break
		}
	}
	if strings.Contains(text, "Done (") && strings.Contains(text, "For help, type") {
		in.mu.Lock()
		wasStarting := in.state == StateStarting
		in.state = StateRunning
		in.mu.Unlock()
		if wasStarting {
			in.recordEvent("ready", "server finished starting (port %d)", in.cfg.Port)
		}
	}
}

// Command sends one console command, echoing it into the log for the Web UI.
func (in *Instance) Command(line string) error {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil
	}
	if in.cfg.UseDocker {
		return in.dockerSendCommand(line)
	}
	in.log.Write("[Manager] $ " + line)
	return in.sendRaw(line)
}

// sendRaw writes a line to the process stdin without echoing it.
func (in *Instance) sendRaw(line string) error {
	in.mu.Lock()
	stdin := in.stdin
	in.mu.Unlock()
	if stdin == nil {
		return ErrNotRunning
	}
	if _, err := io.WriteString(stdin, line+"\n"); err != nil {
		return fmt.Errorf("write to console: %w", err)
	}
	return nil
}

// buildArgs assembles the java command line. cfg.Command holds the loader
// specific tail (classpath + main class, or a parsed Forge run script); the
// heap and GC flags are always managed by the manager.
func (in *Instance) buildArgs() []string {
	args := in.memoryArgs()
	args = append(args,
		"-XX:+UseG1GC",
		"-XX:+ParallelRefProcEnabled",
		"-Dfile.encoding=UTF-8",
	)
	args = append(args, in.cfg.ExtraArgs...)
	if len(in.cfg.Command) > 0 {
		args = append(args, in.cfg.Command...)
	} else {
		args = append(args, "-jar", in.jarName(), "nogui")
	}
	return args
}
