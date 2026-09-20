// Package instance models one running Minecraft server: its lifecycle, its
// console, crash detection, auto-restart and status reporting.
package instance

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"minemanager/internal/config"
	"minemanager/internal/logger"
	"minemanager/internal/modpack"
	"minemanager/internal/rcon"
	"minemanager/internal/versions"
)

// State is the current lifecycle phase of an instance.
type State string

const (
	StateStopped    State = "stopped"
	StateInstalling State = "installing"
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateStopping   State = "stopping"
	StateRestarting State = "restarting"
	StateCrashed    State = "crashed"
	StateError      State = "error"
)

// Terminal reports whether the state implies the process is gone.
func (s State) Terminal() bool {
	switch s {
	case StateRunning, StateStarting, StateStopping, StateRestarting:
		return false
	}
	return true
}

// fatalPatterns mark a server that is dying or cannot start.
var fatalPatterns = []string{
	"A fatal error has been detected",
	"This crash report has been saved to",
	"Minecraft Crash Report",
	"Exception in thread \"main\"",
	"Could not create the Java Virtual Machine",
	"A fatal exception has occurred",
	"Failed to start the minecraft server",
	"Error: Unable to access jarfile",
	"Invalid or corrupt jarfile",
	"SERVER IS RUNNING IN OFFLINE MODE", // informational, handled elsewhere
	"Cannot assign requested address",
	"Failed to bind to port",
	"Perhaps a server is already running on that port?",
}

// warnPatterns are notable but not necessarily fatal.
var warnPatterns = []string{
	"Can't keep up! Is the server overloaded?",
	"Running the server with a deprecated",
	"_WARNINGS_",
	"User Authenticator",
}

// Event is a manager-level lifecycle entry shown in the Web UI.
type Event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Message string    `json:"message"`
}

// Player is an online player as reported by RCON.
type Player struct {
	Name string `json:"name"`
}

// maxEvents is how many lifecycle events are retained in memory.
const maxEvents = 100

// stableUptime is how long a server must run before its crash streak resets.
const stableUptime = 10 * time.Minute

// Instance is a single Minecraft server installation and its process.
type Instance struct {
	mu       sync.Mutex
	cfg      *config.ServerConfig
	dir      string
	app      *config.AppConfig
	api      *versions.API
	modpacks *modpack.Installer
	log      *logger.Logger

	cmd          *exec.Cmd
	stdin        io.WriteCloser
	done         chan struct{}
	supCtx       context.Context
	supCancel    context.CancelFunc
	startedAt    time.Time
	stopSignaled bool
	crashFlag    bool
	state        State
	restartSeq   int // crash streak for back-off

	rconClient *rcon.Client
	players    []Player
	maxPlayers int

	events    []Event
	installMu sync.Mutex
	closed    chan struct{}
	closeOnce sync.Once
}

// io alias kept for the stdin pipe used across package files.

// New creates an instance handle. The directory is created lazily by Install.
func New(cfg *config.ServerConfig, dir string, app *config.AppConfig,
	api *versions.API, mp *modpack.Installer) (*Instance, error) {

	log, err := logger.New(dir)
	if err != nil {
		return nil, err
	}
	in := &Instance{
		cfg:      cfg,
		dir:      dir,
		app:      app,
		api:      api,
		modpacks: mp,
		log:      log,
		state:    StateStopped,
		closed:   make(chan struct{}),
	}
	in.recordEvent("loaded", "instance registered (%s %s)", cfg.Type, cfg.Version)
	return in, nil
}

// ID, Name, Dir and Cfg expose the immutable identity of the instance.
func (in *Instance) ID() string                { return in.cfg.ID }
func (in *Instance) Name() string              { return in.cfg.Name }
func (in *Instance) Dir() string               { return in.dir }
func (in *Instance) Cfg() *config.ServerConfig { return in.cfg }
func (in *Instance) Logger() *logger.Logger    { return in.log }

// State returns the current lifecycle state.
func (in *Instance) State() State {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.state
}

// IsRunning reports whether the process is alive.
func (in *Instance) IsRunning() bool {
	return !in.State().Terminal()
}

// snapshotState performs an atomic state transition.
func (in *Instance) setState(s State) {
	in.mu.Lock()
	in.state = s
	in.mu.Unlock()
}

// recordEvent appends a lifecycle event and mirrors it to the console log.
func (in *Instance) recordEvent(kind, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	in.mu.Lock()
	in.events = append(in.events, Event{Time: time.Now(), Type: kind, Message: msg})
	if len(in.events) > maxEvents {
		in.events = in.events[len(in.events)-maxEvents:]
	}
	in.mu.Unlock()
	in.log.Write("[Manager] " + msg)
}

// Start provisions the jar if needed and launches the server process.
func (in *Instance) Start(ctx context.Context) error {
	if err := in.prepareToRun(ctx); err != nil {
		in.log.Writef("[Manager] start failed: %v", err)
		return err
	}
	if in.cfg.UseDocker {
		if err := in.launchDocker(ctx); err != nil {
			in.log.Writef("[Manager] start failed: %v", err)
			return err
		}
		return nil
	}
	if err := in.launchNative(ctx); err != nil {
		in.log.Writef("[Manager] start failed: %v", err)
		return err
	}
	return nil
}

// prepareToRun installs missing files and marks the instance as starting.
func (in *Instance) prepareToRun(ctx context.Context) error {
	in.mu.Lock()
	switch in.state {
	case StateStopped, StateCrashed, StateError, StateRestarting:
		// No process is running in any of these states, so a start is safe.
		in.state = StateStarting
	default:
		in.mu.Unlock()
		return fmt.Errorf("server is already %s", in.state)
	}
	in.mu.Unlock()

	if _, err := os.Stat(in.jarPath()); err != nil {
		in.log.Write("server jar missing, installing before start")
		if err := in.Install(ctx); err != nil {
			in.setState(StateError)
			return err
		}
	}
	if err := in.writeEULA(); err != nil {
		in.setState(StateError)
		return err
	}
	if err := in.writeProperties(); err != nil {
		in.setState(StateError)
		return err
	}
	in.setState(StateStarting)
	return nil
}

// Stop asks the server to shut down gracefully, then forces it after a timeout.
func (in *Instance) Stop(ctx context.Context) error {
	if in.cfg.UseDocker {
		return in.stopDocker(ctx)
	}
	in.mu.Lock()
	cmd, done := in.cmd, in.done
	if cmd == nil {
		in.mu.Unlock()
		return ErrNotRunning
	}
	in.stopSignaled = true
	in.mu.Unlock()

	in.setState(StateStopping)
	in.log.Write("[Manager] stopping server")
	_ = in.sendRaw("stop")

	select {
	case <-done:
		return nil
	case <-time.After(60 * time.Second):
		in.log.Write("[Manager] graceful stop timed out, killing process")
		return in.Kill()
	}
}

// Restart performs a clean stop followed by a start.
func (in *Instance) Restart(ctx context.Context) error {
	in.setState(StateRestarting)
	in.recordEvent("restart", "manual restart requested")
	if err := in.Stop(ctx); err != nil && err != ErrNotRunning {
		in.log.Writef("[Manager] restart: stop failed: %v", err)
	}
	return in.Start(ctx)
}

// Kill terminates the process without waiting for a clean save.
func (in *Instance) Kill() error {
	if in.cfg.UseDocker {
		return in.dockerKill()
	}
	in.mu.Lock()
	cmd := in.cmd
	in.stopSignaled = true
	in.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return ErrNotRunning
	}
	in.log.Write("[Manager] killing server process")
	return cmd.Process.Kill()
}

// supervise waits for process exit and decides crash vs. clean stop.
func (in *Instance) supervise(exitCode int) {
	in.mu.Lock()
	cmd := in.cmd
	in.cmd = nil
	if in.stdin != nil {
		_ = in.stdin.Close()
		in.stdin = nil
	}
	if in.rconClient != nil {
		_ = in.rconClient.Close()
		in.rconClient = nil
	}
	in.players = nil
	stopped := in.stopSignaled
	uptime := time.Since(in.startedAt)
	state := in.state
	in.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Release()
	}

	if stopped || state == StateStopping || state == StateRestarting {
		in.log.Writef("[Manager] server stopped (exit %d)", exitCode)
		in.setState(StateStopped)
		in.mu.Lock()
		in.stopSignaled = false
		in.mu.Unlock()
		return
	}

	in.recordEvent("crash", "server exited unexpectedly (code %d) after %s",
		exitCode, uptime.Round(time.Second))
	in.setState(StateCrashed)
	in.maybeRestart(exitCode, uptime)
}

// maybeRestart honours AutoRestart with an exponential back-off and a cap.
func (in *Instance) maybeRestart(exitCode int, uptime time.Duration) {
	in.mu.Lock()
	auto := in.cfg.AutoRestart
	max := in.cfg.MaxRestarts
	if uptime > stableUptime {
		in.restartSeq = 0 // it was stable: this is a fresh incident
	}
	in.restartSeq++
	seq := in.restartSeq
	in.mu.Unlock()

	if !auto {
		in.log.Write("[Manager] auto-restart is disabled; leaving server stopped")
		return
	}
	if max > 0 && seq > max {
		in.log.Writef("[Manager] giving up after %d restart attempts", max)
		in.recordEvent("restart", "auto-restart gave up after %d attempts", max)
		return
	}

	backoff := time.Duration(seq) * 5 * time.Second
	if backoff > 60*time.Second {
		backoff = 60 * time.Second
	}
	in.setState(StateRestarting)
	in.recordEvent("restart", "auto-restart attempt %d in %s", seq, backoff)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	select {
	case <-time.After(backoff):
	case <-in.closed:
		return
	case <-ctx.Done():
		return
	}
	if err := in.Start(ctx); err != nil {
		in.log.Writef("[Manager] auto-restart failed: %v", err)
		in.setState(StateError)
	}
}

// Close releases resources held by a stopped instance handle. It also cancels
// any pending auto-restart so the server is not resurrected during shutdown.
func (in *Instance) Close() {
	in.closeOnce.Do(func() { close(in.closed) })
	in.mu.Lock()
	if in.rconClient != nil {
		_ = in.rconClient.Close()
		in.rconClient = nil
	}
	in.mu.Unlock()
	in.log.Close()
}

// jarName resolves the runnable jar file name for this instance.
func (in *Instance) jarName() string {
	if in.cfg.JARName != "" {
		return in.cfg.JARName
	}
	switch in.cfg.Type {
	case config.TypeVanilla:
		return fmt.Sprintf("minecraft_server.%s.jar", in.cfg.Version)
	case config.TypePaper:
		return fmt.Sprintf("paper-%s-%d.jar", in.cfg.Version, in.cfg.Build)
	case config.TypePurpur:
		return fmt.Sprintf("purpur-%s-%d.jar", in.cfg.Version, in.cfg.Build)
	case config.TypeFabric:
		return fmt.Sprintf("fabric-server-mc.%s-loader.%s-launch.jar",
			in.cfg.Version, in.cfg.LoaderVersion)
	case config.TypeForge:
		return fmt.Sprintf("forge-%s.jar", in.cfg.Version)
	case config.TypeNeoForge:
		return fmt.Sprintf("neoforge-%s.jar", in.cfg.Version)
	}
	return "server.jar"
}

func (in *Instance) jarPath() string { return filepath.Join(in.dir, in.jarName()) }

// javaBinary locates a JVM suitable for this server's game version and loader.
// Explicit overrides win; otherwise the newest installed JVM inside the allowed
// range is used. Forge must not run on a too-new JDK because it bundles an ASM
// version that cannot read newer class files, so it is capped — and if no JVM
// in range exists, a Temurin JDK is provisioned into <data>/java automatically.
func (in *Instance) javaBinary() (string, error) {
	if in.cfg.JavaPath != "" {
		return in.cfg.JavaPath, nil
	}
	if in.app.DefaultJava != "" {
		return in.app.DefaultJava, nil
	}
	if p, err := exec.LookPath("java"); err == nil {
		return p, nil
	}

	need := javaMajor(in.cfg.Version)
	max := javaMax(in.cfg.Type, in.cfg.Version)
	candidates := findJVMs(in.app.DataDir)
	if p, ok := pickJVM(candidates, need, max); ok {
		return p, nil
	}

	target := max
	if target == 0 || target < need {
		target = need
	}
	in.log.Writef("[Manager] no local JVM fits %s %s (needs Java %d, cap %s)",
		in.cfg.Type, in.cfg.Version, need, capLabel(max))
	p, err := in.provisionJVM(target)
	if err != nil {
		return "", fmt.Errorf("java %d-%s not found and provisioning failed: %w",
			need, capLabel(max), err)
	}
	return p, nil
}

func capLabel(max int) string {
	if max == 0 {
		return "none"
	}
	return fmt.Sprint(max)
}

// pickJVM selects the best candidate for the [need, max] range; max == 0 means
// uncapped. It only reports a JVM it is confident about, so that a missing fit
// triggers provisioning instead of a launch that is doomed to fail.
func pickJVM(candidates []jvmCandidate, need, max int) (string, bool) {
	if len(candidates) == 0 {
		return "", false
	}
	sorted := make([]jvmCandidate, len(candidates))
	copy(sorted, candidates)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].major < sorted[j].major })

	for i := len(sorted) - 1; i >= 0; i-- {
		c := sorted[i]
		if c.major >= need && (max == 0 || c.major <= max) {
			return c.path, true
		}
	}
	return "", false
}

// findJVMs scans the common installation roots plus the manager's own
// provisioned runtimes and returns every java binary it can find together with
// the parsed major version of its distribution.
func findJVMs(dataDir string) []jvmCandidate {
	var roots []string
	home := os.Getenv("ProgramFiles")
	if home == "" {
		home = `C:\Program Files`
	}
	home86 := os.Getenv("ProgramFiles(x86)")
	if home86 == "" {
		home86 = `C:\Program Files (x86)`
	}
	roots = append(roots,
		filepath.Join(home, "Eclipse Adoptium"),
		filepath.Join(home, "Java"),
		filepath.Join(home, "Zulu"),
		filepath.Join(home, "Microsoft"),
		filepath.Join(home, "BellSoft"),
		filepath.Join(home, "Semeru"),
		filepath.Join(home86, "Java"),
		filepath.Join(home86, "Eclipse Adoptium"),
		`/usr/lib/jvm`,
		`/opt/java`,
		filepath.Join(dataDir, "java"),
	)

	var out []jvmCandidate
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			major := parseJVMajor(e.Name())
			for _, exe := range []string{"java.exe", "java"} {
				bin := filepath.Join(root, e.Name(), "bin", exe)
				if _, err := os.Stat(bin); err == nil {
					out = append(out, jvmCandidate{path: bin, major: major})
					break
				}
			}
		}
	}

	// The Oracle "javapath" shim points at the registry-configured JVM; its
	// version is unknown, so it is only a last resort.
	shim := filepath.Join(home, "Common Files", "Oracle", "Java", "javapath", "java.exe")
	if _, err := os.Stat(shim); err == nil {
		out = append(out, jvmCandidate{path: shim, major: 0})
	}
	return out
}

// parseJVMajor extracts the major version from a distribution directory name,
// e.g. "jdk-27" -> 27, "jre1.8.0_503" -> 8, "jdk8u302" -> 8, "zulu21.34" -> 21.
func parseJVMajor(name string) int {
	s := strings.ToLower(name)
	for _, prefix := range []string{"jdk", "jre", "java", "zulu", "temurin", "adoptium", "openjdk"} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimLeft(s, "-_.")

	if strings.HasPrefix(s, "1.") {
		parts := strings.SplitN(s, ".", 3)
		if len(parts) > 1 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				return n // 1.8 -> 8
			}
		}
	}
	var sb strings.Builder
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		sb.WriteRune(r)
	}
	n, _ := strconv.Atoi(sb.String())
	return n
}

// jvmCandidate is an installed JVM discovered on disk.
type jvmCandidate struct {
	path  string
	major int
}

// javaMajor picks the minimum JVM generation that supports a game version.
func javaMajor(game string) int {
	switch {
	case len(game) >= 6 && game[:6] >= "1.20.5":
		return 21
	case len(game) >= 4 && game[:4] >= "1.17":
		return 17
	case len(game) >= 4 && game[:4] >= "1.16":
		return 16
	}
	return 8
}

// javaMax is the highest JVM generation that is known to work with a loader.
// Forge and NeoForge bundle their own ASM, which refuses class files newer than
// the version it was built against, so an uncapped "newest wins" pick breaks
// them. Other loaders run fine on current JVMs, so they are uncapped.
func javaMax(typ config.ServerType, game string) int {
	if typ != config.TypeForge && typ != config.TypeNeoForge {
		return 0
	}
	switch {
	case len(game) >= 4 && game[:4] >= "1.17":
		return 21
	}
	return 16
}

// memoryArgs builds the -Xmx/-Xms pair from the configured memory budget.
func (in *Instance) memoryArgs() []string {
	mem := in.cfg.MemoryMB
	if mem <= 0 {
		mem = 2048
	}
	xms := mem / 2
	if xms < 512 {
		xms = 512
	}
	return []string{
		fmt.Sprintf("-Xmx%dM", mem),
		fmt.Sprintf("-Xms%dM", xms),
	}
}
