# Minecraft Server Manager

A single-binary, self-hosted control plane for Minecraft dedicated servers, with
a built-in Web UI. Written in Go, no runtime dependencies beyond the JVM you
already need for the game itself.

**No compiler needed:** prebuilt binaries for Windows and Linux are committed to
the repository root (`minemanager-windows-amd64.exe`, `minemanager-linux-amd64`).
Just `git clone` and run — see [Quick start](#quick-start).

```
Minecraft Manager
├── Survival          Paper 1.21.4 · 6 GB · :25565
└── Modded            Forge 1.20.1 · 10 GB · :25566
```

## Features

| Area | What it does |
| --- | --- |
| **Server types** | Vanilla, Paper, Purpur, Fabric, Forge, NeoForge — each with its own upstream metadata API and installer |
| **Server icons** | 40 preset pixel-art block textures, or any uploaded image (up to `max_upload_mb` MB) |
| **Create server** | Pick version → loader → RAM → modpack → domain; the manager downloads the jar, accepts the EULA, writes `server.properties` and installs the modpack |
| **Console** | Live terminal in the browser over SSE, with full history, command input and log-level colouring |
| **RCON** | Full Source RCON client used for commands, player list, whitelist, kick/ban and announcements |
| **Crash detection** | Console signatures (`A fatal error has been detected`, `Failed to bind to port`, …) plus unexpected-exit detection |
| **Auto restart** | Configurable restart-on-crash with exponential back-off and a streak cap that resets after a stable uptime |
| **Backups** | Zip archives of the server (world + configs, caches excluded), `save-all flush` before capture when online, retention policy, one-click restore |
| **Scheduler** | Per-server recurring jobs: restart/backup every N hours or daily at `HH:MM` |
| **Whitelist** | Add/remove/on/off via RCON when running, or direct `whitelist.json` editing (offline-mode UUIDs) when stopped |
| **Players online** | Live player list with kick / ban actions |
| **Modpacks** | Installs Modrinth packs (downloads every declared mod) and `overrides/`-style zips |
| **Updates** | Re-resolves the latest Paper/Purpur build for the pinned version and swaps the jar |
| **systemd** | Generates a ready-to-use unit with the same flags and restart policy |
| **Docker** | Runs the server inside a container, or exports a `docker-compose.yml` |
| **Web UI** | Sidebar dashboard, console, players, backups, settings and deploy tabs — all embedded in the binary |

## Quick start

### Option 1: run a prebuilt binary (no Go toolchain needed)

Prebuilt binaries are committed at the repository root — clone and run:

```bash
git clone https://github.com/MrClerkSmith/minemanager.git
cd minemanager
```

**Windows** (PowerShell):

```powershell
.\minemanager-windows-amd64.exe -data .\data -addr 127.0.0.1:8080
```

**Linux** (amd64):

```bash
chmod +x minemanager-linux-amd64
./minemanager-linux-amd64 -data ./data -addr 127.0.0.1:8080
```

The `data/` directory (config, servers, backups) is created on first start.

### Option 2: build from source

```bash
# build (Go 1.21+)
go build -o minemanager .

# run: the data directory holds config, servers and backups
./minemanager -data ./data -addr 127.0.0.1:8080
```

Cross-compile from any platform:

```bash
GOOS=linux   GOARCH=amd64 go build -o minemanager-linux-amd64 .
GOOS=windows GOARCH=amd64 go build -o minemanager-windows-amd64.exe .
```

Open <http://127.0.0.1:8080>. On first start the manager seeds two example
servers — **Survival** (Paper) and **Modded** (Forge) — in the stopped state.
Click a server, then **Start**; the jar is downloaded and launched automatically.

### Serving the panel on another IP address

By default the panel listens on `127.0.0.1`, which is reachable **only from the
host itself**. To open it to your LAN or to a remote Linux server, bind a
different address with `-addr`:

```bash
# listen on every interface (LAN + public IP) — the common case for a VPS
./minemanager -data ./data -addr 0.0.0.0:8080

# listen on one specific interface only
./minemanager -data ./data -addr 192.168.1.10:8080
```

Then open `http://<server-ip>:8080` from any browser. Check the reported address
on a remote host with:

```bash
hostname -I          # Linux: shows the machine's LAN/public IPs
ip a | grep inet
```

On Linux, open the firewall port if needed:

```bash
sudo ufw allow 8080/tcp
# or, with firewalld:
sudo firewall-cmd --add-port=8080/tcp --permanent && sudo firewall-cmd --reload
```

> **Warning: no built-in authentication.** The panel currently has no login, so
> anything that can reach `-addr` can start/stop your servers and read the
> console. Do **not** bind `0.0.0.0` on an untrusted network. Put it behind a
> reverse proxy with HTTP Basic Auth (nginx/Apache) or restrict the port with a
> firewall to trusted IPs only. Running as a systemd unit with
> `127.0.0.1:8080` + an SSH tunnel is the safest option for remote access.

### Running as a service on Linux

For a permanent installation, run the manager under systemd so it starts on boot
and survives disconnects. Create `/etc/systemd/system/minemanager.service`:

```ini
[Unit]
Description=Minecraft Server Manager
After=network.target

[Service]
Type=simple
User=minecraft
WorkingDirectory=/opt/minemanager
ExecStart=/opt/minemanager/minemanager-linux-amd64 -data /opt/minemanager/data -addr 0.0.0.0:8080
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now minemanager
sudo systemctl status minemanager     # verify it is running
```

The UI also generates a matching unit per server (Deploy tab → systemd) if you
prefer to run individual game servers as units instead.

Flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data` | `./data` | Data directory (config, `servers/`, `backups/`) |
| `-addr` | `127.0.0.1:8080` | HTTP listen address for the Web UI and API |
| `-debug` | off | Verbose logging |

### Requirements for actual servers

* **A JDK/JRE**. The manager scans the usual installation roots (`Program Files\Java`,
  Adoptium, Zulu, Microsoft, BellSoft, `Program Files (x86)`, `/usr/lib/jvm`,
  `/opt/java` and the Oracle `javapath` shim), parses each distribution's version,
  and picks the newest JVM that fits the game version (Java 21 for 1.20.5+, 17 for
  1.17–1.20.4). Forge is additionally capped at Java 21 because it bundles an ASM
  version that cannot read newer class files. If no installed JVM fits, the manager
  **downloads a Temurin JDK** into `data/java` automatically and uses that. Override
  per server with `java_path` in the Settings tab, or globally with `default_java`
  in `data/config.json`.
* **Network access** to the upstream metadata hosts:
  `piston-meta.mojang.com` (Vanilla), `fill.papermc.io` (Paper),
  `api.purpurmc.org` (Purpur), `meta.fabricmc.net` + `maven.fabricmc.net`
  (Fabric), `maven.minecraftforge.net` (Forge),
  `maven.neoforged.net` (NeoForge). Paper's service requires a
  descriptive `User-Agent`; the manager sends one, configurable as `user_agent`
  in `data/config.json`.
* **Forge** and **NeoForge** installs execute the official installer jar, so
  they need Java at install time. **Fabric** is assembled natively by the
  manager (vanilla jar + loader libraries downloaded straight from Maven,
  launched through `KnotServer`), so it never runs an installer and works on
  hosts where the JVM's TLS stack is locked down. Vanilla/Paper/Purpur are
  plain downloads.

## Data layout

```
data/
├── config.json              # manager settings
├── servers/
│   ├── survival/
│   │   ├── manager.json     # this server's configuration
│   │   ├── eula.txt         # accepted on install
│   │   ├── server.properties# managed keys rewritten, others preserved
│   │   ├── paper-*.jar      # downloaded on install
│   │   └── logs/latest.log  # console mirror
│   └── modded/
└── backups/<id>/*.zip       # archives with timestamps
```

## HTTP API summary

| Method & path | Purpose |
| --- | --- |
| `GET /api/servers` | List every server with status, players, events, backups |
| `POST /api/servers` | Create (and optionally install) a server |
| `GET /api/servers/{id}` | One server's status |
| `PUT /api/servers/{id}` | Update settings / schedule |
| `DELETE /api/servers/{id}?keep=true` | Delete (`keep` preserves the files) |
| `POST /api/servers/{id}/{action}` | `start` `stop` `restart` `kill` `install` `update` `backup` |
| `GET /api/servers/{id}/console` | SSE live console stream |
| `GET /api/servers/{id}/console/history` | Buffered console lines |
| `POST /api/servers/{id}/command` | Send a console command |
| `GET /api/servers/{id}/players` | Online players (RCON) |
| `POST /api/servers/{id}/players/{name}/{kick\|ban\|pardon}` | Player moderation |
| `GET/POST /api/servers/{id}/whitelist` | List / mutate the whitelist |
| `GET /api/servers/{id}/backups` | Archive list |
| `GET /api/servers/{id}/backups/{name}` | Download an archive |
| `POST /api/servers/{id}/backups/{name}/restore` | Restore over the server (must be stopped) |
| `GET /api/servers/{id}/export/{systemd\|docker}` | Deployment files |
| `GET /api/versions?type=&game=` | Upstream version / build lists for the create form |
| `GET /api/types`, `GET /api/config` | Supported loaders and manager settings |

Example — the whole "create server" flow from the plan:

```bash
curl -X POST http://127.0.0.1:8080/api/servers \
  -H 'Content-Type: application/json' \
  -d '{"name":"Survival","type":"paper","version":"1.21.4","memory_mb":6144,
       "domain":"mc.example.com","install_now":true}'
```

The manager allocates a free port, generates an RCON password, downloads the
jar, writes the EULA and properties, installs the modpack if one was given, and
leaves the server ready to start.

## Docker and systemd

* **Docker (managed):** enable *Run in Docker* in the server's settings. The
  server runs in an `eclipse-temurin` container with the server directory
  mounted; the console (`docker logs -f`), command input (`docker exec` into
  PID 1 stdin, RCON fallback) and crash detection all keep working.
* **Docker (export):** the Deploy tab renders a `docker-compose.yml`.
* **systemd:** the Deploy tab renders a unit file using exactly the command
  line the manager would use, including the auto-restart policy.

## Architecture

```
main.go                     flags, config load, graceful shutdown
internal/config             app + server config model, JSON persistence
internal/manager            registry, create/delete, port allocation, seeding
internal/instance           lifecycle: install, start/stop, crash detection,
                             auto-restart, console, RCON ops, exports
internal/logger             ring buffer + SSE fan-out + file mirror
internal/rcon               Source RCON protocol (multi-packet reassembly)
internal/versions           Mojang / Paper / Purpur / Fabric / Forge metadata
internal/modpack            Modrinth + overrides-style pack installation
internal/backup             zip archives, listing, retention, restore
internal/sched              interval and daily restart/backup jobs
internal/web                REST API, SSE console, embedded static UI
internal/web/static         index.html, style.css, app.js (go:embed)
```

## Troubleshooting

* **`start failed: java not found`** — install a JDK (17 for 1.17–1.20.4, 21 for
  1.20.5+) or set `java_path` in the server's Settings tab.
* **`Could not create the Java Virtual Machine`** — the JVM rejected a flag, or
  the selected JVM is too old for the game version (e.g. Java 8 for 1.20.5+).
  The manager prefers a JVM that fits and otherwise downloads one into
  `data/java`, so this only happens when provisioning is blocked (no network to
  Adoptium). Install a matching JDK or set `java_path` manually.
* **`Unsupported class file major version`** — the JVM is newer than the loader
  supports. This should not happen for Forge/NeoForge (capped at Java 21); for
  other loaders, pin `java_path` to an older JDK.
* **`502 Bad Gateway` on version lists** — the upstream host for that loader is
  unreachable from this machine; the create form cannot populate versions until
  it is.
* **`cannot edit the whitelist of an online-mode server while it is stopped`** —
  online UUIDs are only known to Mojang; start the server and add the player
  through RCON instead.
* **Forge servers** need the run script the installer writes; the manager
  parses `run.sh`/`run.bat` automatically and stores the resulting command line.
  Fabric servers store a classpath command built from the loader libraries and
  are launched through `KnotServer`.
* **`installer exited with error` (Forge)** — the installer is a java program
  that downloads from the internet. A failure with an SSL exception
  (`NoSuchAlgorithmException: ... SunJSSE`) means the JVM cannot initialise TLS
  in that environment (locked-down or containerised hosts). Fabric is not
  affected: it is assembled by the manager without any installer.
