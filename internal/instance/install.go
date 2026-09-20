// Installation and update logic: artifact download, headless mod-loader
// installers, EULA acceptance, server.properties management and modpacks.
package instance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"mineserver/internal/config"
	"mineserver/internal/versions"
)

const downloadTimeout = 30 * time.Minute

// Install downloads the server jar, runs any required installer, applies the
// modpack if configured and writes the EULA + server.properties.
func (in *Instance) Install(ctx context.Context) error {
	in.installMu.Lock()
	defer in.installMu.Unlock()

	in.setState(StateInstalling)
	in.recordEvent("install", "installing %s %s",
		in.cfg.Type.DisplayName(), in.cfg.Version)
	err := in.installLocked(ctx)
	if err != nil {
		in.recordEvent("install", "install failed: %v", err)
		in.setState(StateError)
		return err
	}
	in.recordEvent("install", "installation complete")
	in.setState(StateStopped)
	return nil
}

// installLocked performs the installation; Install handles state and events.
func (in *Instance) installLocked(ctx context.Context) error {
	if err := os.MkdirAll(in.dir, 0o755); err != nil {
		return err
	}

	art, err := in.api.Resolve(ctx, in.cfg)
	if err != nil {
		return fmt.Errorf("resolve version: %w", err)
	}

	if art.ServerJarURL != "" {
		target := filepath.Join(in.dir, art.ServerJarName)
		if err := in.download(ctx, art.ServerJarURL, target); err != nil {
			return err
		}
		in.cfg.JARName = art.ServerJarName
	}

	if len(art.Libraries) > 0 {
		if err := in.installLibraries(ctx, art); err != nil {
			return err
		}
	}

	if art.InstallerURL != "" {
		if err := in.runInstaller(ctx, art); err != nil {
			return err
		}
	}

	if in.cfg.ModpackURL != "" {
		in.log.Writef("[Manager] installing modpack: %s", in.cfg.ModpackURL)
		summary, err := in.modpacks.Install(ctx, in.cfg.ModpackURL, in.dir,
			func(format string, args ...any) { in.log.Write("[modpack] " + fmt.Sprintf(format, args...)) })
		if err != nil {
			return fmt.Errorf("modpack: %w", err)
		}
		in.recordEvent("modpack", summary)
	}

	if err := in.writeEULA(); err != nil {
		return err
	}
	if err := in.writeProperties(); err != nil {
		return err
	}
	return in.cfg.Save(in.dir)
}

// installLibraries downloads the runtime dependencies declared by the loader
// metadata directly into the server directory and records the classpath launch
// command. Files already present are kept, so this is also used by Update.
func (in *Instance) installLibraries(ctx context.Context, art *versions.Artifact) error {
	classpath := []string{in.jarName()}
	downloaded := 0
	for _, lib := range art.Libraries {
		target := filepath.Join(in.dir, filepath.FromSlash(lib.Path))
		if _, err := os.Stat(target); err != nil {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := in.download(ctx, lib.URL, target); err != nil {
				return err
			}
			downloaded++
		}
		classpath = append(classpath, filepath.FromSlash(lib.Path))
	}
	if art.MainClass != "" {
		// The game jar is on the classpath, so the loader locates it from there;
		// --gameJar would be forwarded to the game itself and rejected.
		in.cfg.Command = []string{
			"-cp",
			strings.Join(classpath, string(os.PathListSeparator)),
			art.MainClass,
			"nogui",
		}
		in.recordEvent("install", "assembled loader: %d libraries (%d downloaded), main class %s",
			len(art.Libraries), downloaded, art.MainClass)
	}
	return nil
}

// Update re-resolves the latest build for the pinned game version and swaps
// the server jar. The server must be stopped.
func (in *Instance) Update(ctx context.Context) error {
	if !in.State().Terminal() {
		return ErrAlreadyRunning
	}
	in.recordEvent("update", "checking for updates")

	switch in.cfg.Type {
	case config.TypePaper, config.TypePurpur:
		builds, err := in.api.Builds(ctx, in.cfg.Type, in.cfg.Version)
		if err != nil || len(builds) == 0 {
			return fmt.Errorf("list builds: %w", err)
		}
		if in.cfg.Build > 0 && builds[0].Build <= in.cfg.Build {
			in.log.Writef("[Manager] already on the latest build (%d)", in.cfg.Build)
			return nil
		}
		in.log.Writef("[Manager] updating build %d -> %d", in.cfg.Build, builds[0].Build)
		old := in.jarPath()
		in.cfg.Build = builds[0].Build
		in.cfg.JARName = ""
		if err := in.Install(ctx); err != nil {
			return err
		}
		if old != in.jarPath() {
			_ = os.Remove(old)
		}
		in.recordEvent("update", "updated to build %d", in.cfg.Build)
		return nil
	default:
		// Vanilla re-downloads the same jar; loaders re-run their installer.
		old := in.jarPath()
		if err := in.Install(ctx); err != nil {
			return err
		}
		if old != in.jarPath() {
			_ = os.Remove(old)
		}
		in.recordEvent("update", "reinstalled latest %s artifacts", in.cfg.Type)
		return nil
	}
}

// runInstaller executes the Fabric / Forge installer headlessly in the server
// directory, streaming its output into the console log.
func (in *Instance) runInstaller(ctx context.Context, art *versions.Artifact) error {
	java, err := in.javaBinary()
	if err != nil {
		return err
	}
	installer := filepath.Join(in.dir, art.InstallerName)
	if err := in.download(ctx, art.InstallerURL, installer); err != nil {
		return err
	}

	var args []string
	switch in.cfg.Type {
	case config.TypeFabric:
		loader := in.cfg.LoaderVersion
		if loader == "" {
			builds, err := in.api.Builds(ctx, config.TypeFabric, in.cfg.Version)
			if err != nil || len(builds) == 0 {
				return fmt.Errorf("resolve fabric loader: %w", err)
			}
			loader = builds[0].Name
			in.cfg.LoaderVersion = loader
		}
		args = []string{"-jar", art.InstallerName, "server", "-mcversion",
			in.cfg.Version, "-loader", loader, "-downloadMinecraft", "-noprofile"}
	case config.TypeForge, config.TypeNeoForge:
		args = []string{"-jar", art.InstallerName, "--installServer"}
	}

	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, java, args...)
	cmd.Dir = in.dir
	in.log.Writef("[Manager] running installer: %s %s", java, strings.Join(args, " "))
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			in.log.Write("[installer] " + line)
		}
	}
	if err != nil {
		return fmt.Errorf("installer exited with error: %w", err)
	}

	if in.cfg.Type == config.TypeForge || in.cfg.Type == config.TypeNeoForge {
		if runArgs := in.parseRunScript(); len(runArgs) > 0 {
			in.cfg.Command = runArgs
			// Modern installs launch through the run script and the libraries
			// it references; there is no top-level server jar, so the run
			// script itself is the instance entry point.
			script := "run.sh"
			if runtime.GOOS == "windows" {
				script = "run.bat"
			}
			in.cfg.JARName = script
			in.recordEvent("install", "parsed %s run script (%d args)", in.cfg.Type, len(runArgs))
		} else {
			in.cfg.Command = []string{"-jar", in.findForgeJar(), "nogui"}
			in.cfg.JARName = in.findForgeJar()
		}
	}
	return nil
}

// parseRunScript extracts the java argv from the run.sh / run.bat that the
// Forge installer writes, which is the only officially supported way to start
// a modern Forge dedicated server.
func (in *Instance) parseRunScript() []string {
	// Prefer the script written for this OS: the classpath separator differs
	// (":" on POSIX, ";" on Windows) and the JVM rejects the foreign one.
	scripts := []string{"run.sh", "run.bat"}
	if runtime.GOOS == "windows" {
		scripts = []string{"run.bat", "run.sh"}
	}
	for _, name := range scripts {
		data, err := os.ReadFile(filepath.Join(in.dir, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "java") {
				continue
			}
			rest := strings.TrimSpace(line[len("java"):])
			if rest == "" {
				continue
			}
			args := splitShellArgs(rest)
			// Drop shell/batch pass-through tokens ("$@", "%*", ...) that mean
			// "forward the caller's arguments" but are literal to the JVM.
			filtered := args[:0]
			for _, a := range args {
				switch strings.Trim(a, "\"'") {
				case "$@", "%*", "$*", "":
					continue
				}
				filtered = append(filtered, a)
			}
			args = filtered
			if len(args) > 0 && args[len(args)-1] != "nogui" {
				args = append(args, "nogui")
			}
			// Normalise any classpath that came from the other platform's
			// script (only touched when it looks like a path list).
			sep := string(os.PathListSeparator)
			foreign := ":"
			if sep == ":" {
				foreign = ";"
			}
			for i, a := range args {
				if strings.Contains(a, foreign) && strings.ContainsAny(a, "/\\") {
					args[i] = strings.ReplaceAll(a, foreign, sep)
				}
			}
			return args
		}
	}
	return nil
}

// findForgeJar falls back to the server jar name when no run script exists.
func (in *Instance) findForgeJar() string {
	if name := in.jarName(); name != "server.jar" {
		return name
	}
	entries, err := os.ReadDir(in.dir)
	if err != nil {
		return "forge-server.jar"
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "forge-") && strings.HasSuffix(e.Name(), "-server.jar") {
			return e.Name()
		}
	}
	return "forge-server.jar"
}

// splitShellArgs performs a minimal POSIX-ish word split honouring quotes.
func splitShellArgs(s string) []string {
	var args []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

// download fetches url into target with a .part intermediate file.
func (in *Instance) download(ctx context.Context, url, target string) error {
	in.log.Writef("[Manager] downloading %s", url)
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", in.app.UserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}

	tmp := target + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	written, err := io.Copy(f, resp.Body)
	if err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("download: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	in.log.Writef("[Manager] saved %s (%s)", filepath.Base(target), humanBytes(written))
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// writeEULA accepts the Mojang EULA so the server can actually start.
func (in *Instance) writeEULA() error {
	content := "# By changing the setting below to TRUE you are indicating your agreement to our EULA (https://aka.ms/MinecraftEULA).\neula=true\n"
	return os.WriteFile(filepath.Join(in.dir, "eula.txt"), []byte(content), 0o644)
}

// managedProps are the keys the manager owns; everything else in
// server.properties is preserved across restarts so users can tune the world.
var managedProps = []string{
	"server-port", "motd", "white-list", "enforce-whitelist",
	"online-mode", "enable-rcon", "rcon.port", "rcon.password", "enable-query",
}

// writeProperties merges the manager owned settings into server.properties.
func (in *Instance) writeProperties() error {
	path := filepath.Join(in.dir, "server.properties")
	props := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				props[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}

	rconPort := in.cfg.RCONPort
	if rconPort == 0 {
		rconPort = in.cfg.Port + 10
	}
	motd := in.cfg.MOTD
	if motd == "" {
		motd = fmt.Sprintf("%s - %s %s", in.cfg.Name, in.cfg.Type.DisplayName(), in.cfg.Version)
	}
	for k, v := range map[string]string{
		"server-port":       strconv.Itoa(in.cfg.Port),
		"motd":              motd,
		"white-list":        strconv.FormatBool(in.cfg.Whitelist),
		"enforce-whitelist": strconv.FormatBool(in.cfg.Whitelist),
		"online-mode":       strconv.FormatBool(in.cfg.OnlineMode),
		"enable-rcon":       "true",
		"rcon.port":         strconv.Itoa(rconPort),
		"rcon.password":     in.cfg.RCONPassword,
		"enable-query":      "true",
	} {
		props[k] = v
	}

	var rest []string
	for k := range props {
		if !contains(managedProps, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)

	var b strings.Builder
	b.WriteString("# Managed by Minecraft Manager; managed keys are rewritten on every start,\n")
	b.WriteString("# all other keys are preserved. Edit those to tune your world.\n")
	for _, k := range managedProps {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(props[k])
		b.WriteByte('\n')
	}
	for _, k := range rest {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(props[k])
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
