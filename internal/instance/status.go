// Status snapshots exposed over the management API.
package instance

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"mineserver/internal/backup"
	"mineserver/internal/config"
)

// Status is the full public view of one instance, sent to the Web UI.
type Status struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Type          string           `json:"type"`
	TypeLabel     string           `json:"type_label"`
	Version       string           `json:"version"`
	LoaderVersion string           `json:"loader_version"`
	MemoryMB      int              `json:"memory_mb"`
	Port          int              `json:"port"`
	Domain        string           `json:"domain"`
	State         State            `json:"state"`
	PID           int              `json:"pid"`
	Uptime        float64          `json:"uptime"`
	Online        int              `json:"online"`
	MaxPlayers    int              `json:"max_players"`
	Players       []Player         `json:"players"`
	Whitelist     bool             `json:"whitelist"`
	AutoRestart   bool             `json:"auto_restart"`
	MaxRestarts   int              `json:"max_restarts"`
	UseDocker     bool             `json:"use_docker"`
	MOTD          string           `json:"motd"`
	JavaPath      string           `json:"java_path"`
	ModpackURL    string           `json:"modpack_url"`
	Icon          string           `json:"icon"`
	IconImage     bool             `json:"icon_image"`
	CreatedAt     time.Time        `json:"created_at"`
	Directory     string           `json:"directory"`
	Events        []Event          `json:"events"`
	Backups       []backup.Info    `json:"backups"`
	Schedule      *config.Schedule `json:"schedule"`
}

// Status returns a point-in-time snapshot of the instance.
func (in *Instance) Status() Status {
	in.mu.Lock()
	defer in.mu.Unlock()

	var uptime float64
	pid := 0
	if !in.state.Terminal() {
		uptime = time.Since(in.startedAt).Seconds()
	}
	if in.cmd != nil && in.cmd.Process != nil {
		pid = in.cmd.Process.Pid
	}

	players := in.players
	if players == nil {
		players = []Player{}
	}
	events := in.events
	if events == nil {
		events = []Event{}
	}
	backups, _ := backup.List(backupDir(in))

	return Status{
		ID:            in.cfg.ID,
		Name:          in.cfg.Name,
		Type:          string(in.cfg.Type),
		TypeLabel:     in.cfg.Type.DisplayName(),
		Version:       in.cfg.Version,
		LoaderVersion: in.cfg.LoaderVersion,
		MemoryMB:      in.cfg.MemoryMB,
		Port:          in.cfg.Port,
		Domain:        in.cfg.Domain,
		State:         in.state,
		PID:           pid,
		Uptime:        uptime,
		Online:        len(players),
		MaxPlayers:    in.maxPlayers,
		Players:       players,
		Whitelist:     in.cfg.Whitelist,
		AutoRestart:   in.cfg.AutoRestart,
		MaxRestarts:   in.cfg.MaxRestarts,
		UseDocker:     in.cfg.UseDocker,
		MOTD:          in.cfg.MOTD,
		JavaPath:      in.cfg.JavaPath,
		ModpackURL:    in.cfg.ModpackURL,
		Icon:          in.cfg.Icon,
		IconImage:     in.iconImage(),
		CreatedAt:     in.cfg.CreatedAt,
		Directory:     in.dir,
		Events:        events,
		Backups:       backups,
		Schedule:      in.cfg.Schedule,
	}
}

// backupDir is the archive directory for an instance.
func backupDir(in *Instance) string {
	return filepath.Join(in.app.DataDir, "backups", in.cfg.ID)
}

// iconImage reports whether a custom icon image was uploaded for this instance.
func (in *Instance) iconImage() bool {
	_, ok := in.IconFile()
	return ok
}

// IconFile returns the path of a custom uploaded icon, if any. Any file named
// icon.<ext> in the server directory counts.
func (in *Instance) IconFile() (string, bool) {
	entries, err := os.ReadDir(in.dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "icon.") && isImageExt(name) {
			return filepath.Join(in.dir, name), true
		}
	}
	return "", false
}

// RemoveIcon deletes an uploaded icon, if any.
func (in *Instance) RemoveIcon() error {
	path, ok := in.IconFile()
	if !ok {
		return nil
	}
	return os.Remove(path)
}

// isImageExt reports whether a file name has an image extension the manager
// accepts for server icons.
func isImageExt(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp":
		return true
	}
	return false
}
