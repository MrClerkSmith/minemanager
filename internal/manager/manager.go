// Package manager is the top level registry: it owns the instance directory,
// creates and deletes servers, allocates ports and drives the scheduler.
package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mineserver/internal/config"
	"mineserver/internal/instance"
	"mineserver/internal/modpack"
	"mineserver/internal/sched"
	"mineserver/internal/versions"
)

// firstPort is where port allocation starts scanning.
const firstPort = 25565

// Manager holds every loaded instance and the shared services they use.
type Manager struct {
	cfg      *config.AppConfig
	log      *slog.Logger
	api      *versions.API
	modpacks *modpack.Installer
	sched    *sched.Scheduler

	mu        sync.RWMutex
	instances map[string]*instance.Instance
}

// New loads every instance found under <data>/servers.
func New(cfg *config.AppConfig) (*Manager, error) {
	logger := slog.Default().With("component", "manager")
	m := &Manager{
		cfg:       cfg,
		log:       logger,
		api:       versions.New(cfg.UserAgent),
		modpacks:  modpack.New(),
		sched:     sched.New(logger),
		instances: make(map[string]*instance.Instance),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// load reads the on-disk instances, seeding the two example servers from the
// project plan when the data directory is still empty.
func (m *Manager) load() error {
	serversDir := filepath.Join(m.cfg.DataDir, "servers")
	if err := os.MkdirAll(serversDir, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(serversDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := m.loadOne(filepath.Join(serversDir, e.Name())); err != nil {
			m.log.Warn("skipping invalid server directory", "dir", e.Name(), "err", err)
		}
	}

	if len(m.instances) == 0 {
		m.log.Info("no servers found, seeding the example servers")
		if err := m.seed(); err != nil {
			m.log.Warn("seeding example servers failed", "err", err)
		}
	}
	return nil
}

func (m *Manager) loadOne(dir string) error {
	path := filepath.Join(dir, "manager.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config.ServerConfig
	if err := strictUnmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if !cfg.Type.IsValid() || cfg.Version == "" {
		return fmt.Errorf("invalid server config in %s", path)
	}
	if cfg.ID == "" {
		cfg.ID = filepath.Base(dir)
	}
	return m.register(&cfg, dir)
}

func (m *Manager) register(cfg *config.ServerConfig, dir string) error {
	in, err := instance.New(cfg, dir, m.cfg, m.api, m.modpacks)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.instances[cfg.ID]; exists {
		return fmt.Errorf("duplicate server id %q", cfg.ID)
	}
	m.instances[cfg.ID] = in
	m.sched.Set(instanceAdapter{in})
	return nil
}

// seed creates the two example servers shown in the project plan.
func (m *Manager) seed() error {
	examples := []*config.ServerConfig{
		{
			ID: "survival", Name: "Survival", Type: config.TypePaper,
			Version: "1.21.4", MemoryMB: 6 * 1024, Port: 25565,
			MOTD: "Survival - Paper 1.21.4", AutoRestart: true,
			MaxRestarts: 5, CreatedAt: time.Now(),
			Schedule: &config.Schedule{BackupEvery: "12h"},
		},
		{
			ID: "modded", Name: "Modded", Type: config.TypeForge,
			Version: "1.20.1", MemoryMB: 10 * 1024, Port: 25566,
			MOTD: "Modded - Forge 1.20.1", AutoRestart: true,
			MaxRestarts: 5, CreatedAt: time.Now(),
			Schedule: &config.Schedule{RestartEvery: "24h"},
		},
	}
	for _, cfg := range examples {
		cfg.RCONPassword = randomPassword()
		dir := filepath.Join(m.cfg.DataDir, "servers", cfg.ID)
		if err := cfg.Save(dir); err != nil {
			return err
		}
		if err := m.register(cfg, dir); err != nil {
			return err
		}
	}
	return nil
}

// CreateRequest is the payload accepted by Create.
type CreateRequest struct {
	Name          string            `json:"name"`
	Type          config.ServerType `json:"type"`
	Version       string            `json:"version"`
	Build         int               `json:"build"`
	LoaderVersion string            `json:"loader_version"`
	MemoryMB      int               `json:"memory_mb"`
	Port          int               `json:"port"`
	Domain        string            `json:"domain"`
	ModpackURL    string            `json:"modpack_url"`
	OnlineMode    bool              `json:"online_mode"`
	Whitelist     bool              `json:"whitelist"`
	AutoRestart   bool              `json:"auto_restart"`
	MaxRestarts   int               `json:"max_restarts"`
	UseDocker     bool              `json:"use_docker"`
	JavaPath      string            `json:"java_path"`
	Schedule      *config.Schedule  `json:"schedule"`
	InstallNow    bool              `json:"install_now"`
}

// Create provisions a new server directory and registers the instance.
func (m *Manager) Create(ctx context.Context, req *CreateRequest) (*instance.Instance, error) {
	if strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	if !req.Type.IsValid() {
		return nil, fmt.Errorf("unsupported type %q", req.Type)
	}
	if req.Version == "" {
		return nil, fmt.Errorf("version is required")
	}

	id := m.allocateID(req.Name)
	cfg := &config.ServerConfig{
		ID:            id,
		Name:          strings.TrimSpace(req.Name),
		Type:          req.Type,
		Version:       req.Version,
		Build:         req.Build,
		LoaderVersion: req.LoaderVersion,
		MemoryMB:      orDefault(req.MemoryMB, 4096),
		Port:          m.allocatePort(req.Port),
		RCONPassword:  randomPassword(),
		MOTD:          fmt.Sprintf("%s - %s %s", req.Name, req.Type.DisplayName(), req.Version),
		OnlineMode:    true,
		Whitelist:     req.Whitelist,
		Domain:        req.Domain,
		ModpackURL:    req.ModpackURL,
		AutoRestart:   req.AutoRestart,
		MaxRestarts:   orDefault(req.MaxRestarts, 5),
		UseDocker:     req.UseDocker,
		JavaPath:      req.JavaPath,
		Schedule:      req.Schedule,
		CreatedAt:     time.Now(),
	}

	dir := filepath.Join(m.cfg.DataDir, "servers", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := cfg.Save(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	in, err := instance.New(cfg, dir, m.cfg, m.api, m.modpacks)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	m.mu.Lock()
	m.instances[id] = in
	m.mu.Unlock()
	m.sched.Set(instanceAdapter{in})

	if req.InstallNow {
		go func() {
			// Detached context: the install must outlive this HTTP request.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := in.Install(ctx); err != nil {
				m.log.Warn("install failed", "server", id, "err", err)
			}
		}()
	}
	m.log.Info("created server", "id", id, "type", req.Type, "version", req.Version,
		"memory_mb", cfg.MemoryMB, "domain", req.Domain)
	return in, nil
}

// Delete stops a server and removes it. When keepFiles is false the whole
// server directory (world included) is deleted.
func (m *Manager) Delete(ctx context.Context, id string, keepFiles bool) error {
	in, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	if in.IsRunning() {
		if err := in.Stop(ctx); err != nil {
			m.log.Warn("stop before delete failed", "server", id, "err", err)
		}
	}
	in.Close()
	m.mu.Lock()
	delete(m.instances, id)
	m.mu.Unlock()
	m.sched.Remove(id)
	if !keepFiles {
		if err := os.RemoveAll(in.Dir()); err != nil {
			return err
		}
	}
	m.log.Info("deleted server", "id", id, "kept_files", keepFiles)
	return nil
}

// Settings is the editable subset of a server configuration.
type Settings struct {
	Name        string           `json:"name"`
	MemoryMB    int              `json:"memory_mb"`
	Port        int              `json:"port"`
	MOTD        string           `json:"motd"`
	Domain      string           `json:"domain"`
	Whitelist   bool             `json:"whitelist"`
	OnlineMode  bool             `json:"online_mode"`
	AutoRestart bool             `json:"auto_restart"`
	MaxRestarts int              `json:"max_restarts"`
	UseDocker   bool             `json:"use_docker"`
	JavaPath    string           `json:"java_path"`
	ModpackURL  string           `json:"modpack_url"`
	Icon        string           `json:"icon"`
	Schedule    *config.Schedule `json:"schedule"`
}

// ApplySettings updates an instance configuration and persists it.
func (m *Manager) ApplySettings(ctx context.Context, id string, s *Settings) error {
	in, ok := m.Get(id)
	if !ok {
		return ErrNotFound
	}
	if in.IsRunning() {
		return fmt.Errorf("stop the server before changing its settings")
	}
	cfg := in.Cfg()
	if s.Name != "" {
		cfg.Name = s.Name
	}
	if s.MemoryMB > 0 {
		cfg.MemoryMB = s.MemoryMB
	}
	if s.Port > 0 && s.Port != cfg.Port && m.portTaken(s.Port, id) {
		return fmt.Errorf("port %d is already used by another server", s.Port)
	}
	if s.Port > 0 {
		cfg.Port = s.Port
	}
	cfg.MOTD = s.MOTD
	cfg.Domain = s.Domain
	cfg.Whitelist = s.Whitelist
	cfg.OnlineMode = s.OnlineMode
	cfg.AutoRestart = s.AutoRestart
	cfg.MaxRestarts = s.MaxRestarts
	cfg.UseDocker = s.UseDocker
	if s.JavaPath != "" {
		cfg.JavaPath = s.JavaPath
	}
	cfg.ModpackURL = s.ModpackURL
	cfg.Icon = s.Icon
	cfg.Schedule = s.Schedule
	if err := cfg.Save(in.Dir()); err != nil {
		return err
	}
	m.sched.Set(instanceAdapter{in})
	return nil
}

// List returns all instances sorted by name.
func (m *Manager) List() []*instance.Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*instance.Instance, 0, len(m.instances))
	for _, in := range m.instances {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Get fetches one instance by id.
func (m *Manager) Get(id string) (*instance.Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	in, ok := m.instances[id]
	return in, ok
}

// Config returns the application configuration.
func (m *Manager) Config() *config.AppConfig { return m.cfg }

// Versions returns the version API client for the Web UI dropdowns.
func (m *Manager) Versions() *versions.API { return m.api }

// Close stops every server and shuts the scheduler down.
func (m *Manager) Close() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.instances))
	for id := range m.instances {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, id := range ids {
		if in, ok := m.Get(id); ok {
			if in.IsRunning() {
				_ = in.Stop(ctx)
			}
			in.Close()
			m.sched.Remove(id)
		}
	}
}

// allocateID derives a filesystem safe, unique id from a server name.
func (m *Manager) allocateID(name string) string {
	base := strings.ToLower(strings.TrimSpace(name))
	var sb strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	base = strings.Trim(sb.String(), "-")
	if base == "" {
		base = "server"
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	id := base
	for n := 2; ; n++ {
		if _, ok := m.instances[id]; !ok {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}

// allocatePort hands out the requested port or the next free one.
func (m *Manager) allocatePort(want int) int {
	if want > 0 && !m.portTaken(want, "") {
		return want
	}
	for p := firstPort; ; p++ {
		if !m.portTaken(p, "") {
			return p
		}
	}
}

// portTaken reports whether a port is claimed by another instance.
func (m *Manager) portTaken(port int, except string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, in := range m.instances {
		if id == except {
			continue
		}
		if in.Cfg().Port == port {
			return true
		}
		rcon := in.Cfg().RCONPort
		if rcon == 0 {
			rcon = in.Cfg().Port + 10
		}
		if rcon == port {
			return true
		}
	}
	return false
}

func randomPassword() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
