// Deployment exports: a systemd unit for running the server under the OS
// init system, and a docker-compose.yml for running it under Docker.
package instance

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// ExportSystemd returns a systemd unit that runs this server exactly like the
// manager would, including auto-restart policy when enabled.
func (in *Instance) ExportSystemd() string {
	java, err := in.javaBinary()
	if err != nil {
		java = "/usr/bin/java"
	}
	args := in.buildArgs()
	restarts := in.cfg.MaxRestarts
	if restarts <= 0 {
		restarts = 5
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[Unit]\n")
	fmt.Fprintf(&b, "Description=Minecraft Server: %s (%s %s)\n",
		in.cfg.Name, in.cfg.Type.DisplayName(), in.cfg.Version)
	fmt.Fprintf(&b, "After=network.target\n\n")

	fmt.Fprintf(&b, "[Service]\n")
	fmt.Fprintf(&b, "Type=simple\n")
	fmt.Fprintf(&b, "WorkingDirectory=%s\n", in.dir)
	fmt.Fprintf(&b, "ExecStart=%s %s\n", java, strings.Join(args, " "))
	if in.cfg.UseDocker {
		fmt.Fprintf(&b, "# NOTE: this instance is configured for Docker; the unit runs it natively.\n")
	}
	if in.cfg.AutoRestart {
		fmt.Fprintf(&b, "Restart=on-failure\n")
		fmt.Fprintf(&b, "RestartSec=10\n")
		fmt.Fprintf(&b, "StartLimitIntervalSec=600\n")
		fmt.Fprintf(&b, "StartLimitBurst=%d\n", restarts)
	} else {
		fmt.Fprintf(&b, "Restart=no\n")
	}
	fmt.Fprintf(&b, "TimeoutStopSec=60\n")
	fmt.Fprintf(&b, "KillSignal=SIGTERM\n")
	fmt.Fprintf(&b, "SuccessExitStatus=143\n\n")

	fmt.Fprintf(&b, "[Install]\n")
	fmt.Fprintf(&b, "WantedBy=multi-user.target\n")
	return b.String()
}

// ExportDockerCompose returns a docker-compose.yml for this server. It is
// written relative to the server directory so it can be moved next to it.
func (in *Instance) ExportDockerCompose() string {
	args := in.buildArgs()
	quoted := make([]string, 0, len(args))
	for _, a := range args {
		quoted = append(quoted, yamlQuote(a))
	}

	rconPort := in.rconPort()
	var b strings.Builder
	fmt.Fprintf(&b, "# docker-compose.yml for %s (%s %s)\n",
		in.cfg.Name, in.cfg.Type.DisplayName(), in.cfg.Version)
	fmt.Fprintf(&b, "# place next to the server directory and run: docker compose up -d\n\n")
	fmt.Fprintf(&b, "services:\n")
	fmt.Fprintf(&b, "  %s:\n", in.cfg.ID)
	fmt.Fprintf(&b, "    image: %s\n", in.imageName())
	fmt.Fprintf(&b, "    container_name: %s\n", in.containerName())
	fmt.Fprintf(&b, "    working_dir: /server\n")
	fmt.Fprintf(&b, "    volumes:\n")
	fmt.Fprintf(&b, "      - %s:/server\n", yamlQuote(filepath.Clean(in.dir)))
	fmt.Fprintf(&b, "    ports:\n")
	fmt.Fprintf(&b, "      - %q\n", fmt.Sprintf("%d:%d", in.cfg.Port, in.cfg.Port))
	fmt.Fprintf(&b, "      - %q\n", fmt.Sprintf("%d:%d", rconPort, rconPort))
	fmt.Fprintf(&b, "    command: [%s]\n", strings.Join(quoted, ", "))
	fmt.Fprintf(&b, "    stdin_open: true\n")
	if in.cfg.AutoRestart {
		fmt.Fprintf(&b, "    restart: unless-stopped\n")
	} else {
		fmt.Fprintf(&b, "    restart: \"no\"\n")
	}
	if in.cfg.Domain != "" {
		fmt.Fprintf(&b, "    labels:\n")
		fmt.Fprintf(&b, "      - %q\n", fmt.Sprintf("mc.domain=%s", in.cfg.Domain))
	}
	return b.String()
}

// yamlQuote quotes a string for a YAML scalar. strconv.Quote escapes are a
// YAML-compatible subset (\", \\, \n, \t are all valid YAML escapes).
func yamlQuote(s string) string {
	return strconv.Quote(s)
}
