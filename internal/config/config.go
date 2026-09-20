// Package config holds the persistent configuration model for the manager
// itself and for individual Minecraft server instances.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ServerType identifies the server software / mod loader.
type ServerType string

const (
	TypeVanilla  ServerType = "vanilla"
	TypePaper    ServerType = "paper"
	TypePurpur   ServerType = "purpur"
	TypeFabric   ServerType = "fabric"
	TypeForge    ServerType = "forge"
	TypeNeoForge ServerType = "neoforge"
)

// KnownTypes returns the server types the manager knows how to install.
func KnownTypes() []ServerType {
	return []ServerType{TypeVanilla, TypePaper, TypePurpur, TypeFabric, TypeForge, TypeNeoForge}
}

// IsValid reports whether t is a supported server type.
func (t ServerType) IsValid() bool {
	for _, k := range KnownTypes() {
		if t == k {
			return true
		}
	}
	return false
}

// ModLoader reports whether the type is a mod loader requiring an installer step.
func (t ServerType) ModLoader() bool {
	return t == TypeFabric || t == TypeForge || t == TypeNeoForge
}

// DisplayName returns a human friendly label, e.g. "Paper".
func (t ServerType) DisplayName() string {
	switch t {
	case TypeVanilla:
		return "Vanilla"
	case TypePaper:
		return "Paper"
	case TypePurpur:
		return "Purpur"
	case TypeFabric:
		return "Fabric"
	case TypeForge:
		return "Forge"
	case TypeNeoForge:
		return "NeoForge"
	}
	return string(t)
}

// Schedule defines recurring maintenance jobs for an instance.
type Schedule struct {
	// RestartEvery / BackupEvery are durations such as "6h" or "30m".
	// Empty means "not scheduled by interval".
	RestartEvery string `json:"restart_every,omitempty"`
	BackupEvery  string `json:"backup_every,omitempty"`
	// RestartAt / BackupAt are wall clock times in "HH:MM" (24h, local) form.
	RestartAt string `json:"restart_at,omitempty"`
	BackupAt  string `json:"backup_at,omitempty"`
}

// ServerConfig is the on-disk definition of one Minecraft server instance.
// It lives at <data>/servers/<id>/manager.json next to the server files.
type ServerConfig struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Type          ServerType `json:"type"`
	Version       string     `json:"version"`
	LoaderVersion string     `json:"loader_version,omitempty"` // fabric loader / forge build tag
	Build         int        `json:"build,omitempty"`          // paper/purpur build number
	MemoryMB      int        `json:"memory_mb"`                // -Xmx in megabytes
	Port          int        `json:"port"`                     // game port
	RCONPort      int        `json:"rcon_port"`                // 0 => game port + 10
	RCONPassword  string     `json:"rcon_password"`
	MOTD          string     `json:"motd"`
	Whitelist     bool       `json:"whitelist"`
	OnlineMode    bool       `json:"online_mode"`
	Domain        string     `json:"domain,omitempty"`
	ModpackURL    string     `json:"modpack_url,omitempty"`
	Icon          string     `json:"icon,omitempty"` // preset icon name or empty
	AutoRestart   bool       `json:"auto_restart"`
	MaxRestarts   int        `json:"max_restarts"` // per crash streak, 0 => unlimited
	JavaPath      string     `json:"java_path,omitempty"`
	ExtraArgs     []string   `json:"extra_args,omitempty"`
	JARName       string     `json:"jar_name,omitempty"` // resolved server jar file name
	Command       []string   `json:"command,omitempty"`  // full java argv excluding the binary
	UseDocker     bool       `json:"use_docker,omitempty"`
	Schedule      *Schedule  `json:"schedule,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// RCONAddr returns the host:port used to reach the instance's RCON console.
func (c *ServerConfig) RCONAddr(host string) string {
	port := c.RCONPort
	if port == 0 {
		port = c.Port + 10
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// AppConfig is the manager-wide configuration stored at <data>/config.json.
type AppConfig struct {
	DataDir       string `json:"data_dir"`
	Host          string `json:"host"`
	DefaultJava   string `json:"default_java,omitempty"`
	UserAgent     string `json:"user_agent,omitempty"`
	BackupsToKeep int    `json:"backups_to_keep"`
	MaxUploadMB   int    `json:"max_upload_mb"`
}

// Load reads the manager config from dir, creating sane defaults when missing.
func Load(dir string) (*AppConfig, error) {
	path := filepath.Join(dir, "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &AppConfig{
				DataDir:       dir,
				Host:          "127.0.0.1:8080",
				BackupsToKeep: 10,
				MaxUploadMB:   200,
			}, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c AppConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.BackupsToKeep <= 0 {
		c.BackupsToKeep = 10
	}
	if c.MaxUploadMB <= 0 {
		c.MaxUploadMB = 200
	}
	return &c, nil
}

// Save writes the manager config atomically.
func (c *AppConfig) Save() error {
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(c.DataDir, "config.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Save writes the instance config next to the server files.
func (c *ServerConfig) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "manager.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
