// RCON driven operations: player listing, whitelist management, kick/ban and
// server announcements. Whitelist edits fall back to whitelist.json when the
// server is offline.
package instance

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"minemanager/internal/backup"
	"minemanager/internal/rcon"
)

// WhitelistAction selects a whitelist operation.
type WhitelistAction string

const (
	WLAdd     WhitelistAction = "add"
	WLRemove  WhitelistAction = "remove"
	WLEnable  WhitelistAction = "on"
	WLDisable WhitelistAction = "off"
)

// WhitelistEntry mirrors one element of whitelist.json.
type WhitelistEntry struct {
	Name string `json:"name"`
	UUID string `json:"uuid"`
}

// RCON returns a cached, authenticated client, connecting lazily.
func (in *Instance) RCON() (*rcon.Client, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.rconClient != nil {
		return in.rconClient, nil
	}
	if in.state.Terminal() {
		return nil, ErrNotRunning
	}
	addr := in.cfg.RCONAddr("127.0.0.1")
	c, err := rcon.Dial(addr, in.cfg.RCONPassword)
	if err != nil {
		return nil, fmt.Errorf("rcon %s: %w", addr, err)
	}
	in.rconClient = c
	return c, nil
}

// exec sends a command through RCON, returning the server response.
func (in *Instance) exec(cmd string) (string, error) {
	c, err := in.RCON()
	if err != nil {
		return "", err
	}
	return c.Command(cmd)
}

// Players lists the players currently online via RCON "list".
func (in *Instance) Players() ([]Player, error) {
	out, err := in.exec("list")
	if err != nil {
		return nil, err
	}
	_, after, ok := strings.Cut(out, "online:")
	if !ok {
		return nil, nil
	}
	var max int
	if idx := strings.Index(out, "of a max of"); idx >= 0 {
		_, _ = fmt.Sscanf(out[idx:], "of a max of %d", &max)
	}
	var players []Player
	for _, name := range strings.Split(after, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			players = append(players, Player{Name: name})
		}
	}
	in.mu.Lock()
	in.players = players
	in.maxPlayers = max
	in.mu.Unlock()
	return players, nil
}

// Say broadcasts a manager message to every online player.
func (in *Instance) Say(message string) error {
	_, err := in.exec("say " + message)
	return err
}

// Kick disconnects a player.
func (in *Instance) Kick(name string) error {
	_, err := in.exec("kick " + name)
	return err
}

// Ban adds a player to the ban list.
func (in *Instance) Ban(name string) error {
	_, err := in.exec("ban " + name)
	return err
}

// Pardon removes a player from the ban list.
func (in *Instance) Pardon(name string) error {
	_, err := in.exec("pardon " + name)
	return err
}

// Whitelist applies a whitelist operation for the given player name.
func (in *Instance) Whitelist(action WhitelistAction, player string) error {
	if !in.IsRunning() {
		return in.whitelistFile(action, player)
	}
	switch action {
	case WLEnable, WLDisable:
		if _, err := in.exec("whitelist " + string(action)); err != nil {
			return err
		}
	default:
		if _, err := in.exec("whitelist " + string(action) + " " + player); err != nil {
			return err
		}
		if _, err := in.exec("whitelist reload"); err != nil {
			return err
		}
	}
	return nil
}

// whitelistFile mutates whitelist.json while the server is stopped. Only
// offline-mode servers can be edited this way, since an online UUID cannot be
// known without contacting Mojang.
func (in *Instance) whitelistFile(action WhitelistAction, player string) error {
	if action == WLEnable || action == WLDisable {
		in.mu.Lock()
		in.cfg.Whitelist = action == WLEnable
		in.mu.Unlock()
		return in.writeProperties()
	}
	if player == "" {
		return fmt.Errorf("whitelist %s requires a player name", action)
	}
	if in.cfg.OnlineMode {
		return fmt.Errorf("cannot edit the whitelist of an online-mode server while it is stopped; start the server first or disable online-mode")
	}

	path := filepath.Join(in.dir, "whitelist.json")
	entries := in.WhitelistList()
	switch action {
	case WLAdd:
		for _, e := range entries {
			if e.Name == player {
				return nil
			}
		}
		entries = append(entries, WhitelistEntry{
			Name: player,
			UUID: offlineUUID(player),
		})
	case WLRemove:
		next := entries[:0]
		for _, e := range entries {
			if e.Name != player {
				next = append(next, e)
			}
		}
		entries = next
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	in.recordEvent("whitelist", "whitelist %s %s", action, player)
	return nil
}

// WhitelistList reads whitelist.json.
func (in *Instance) WhitelistList() []WhitelistEntry {
	data, err := os.ReadFile(filepath.Join(in.dir, "whitelist.json"))
	if err != nil {
		return nil
	}
	var entries []WhitelistEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	return entries
}

// offlineUUID reproduces the UUID an offline-mode server assigns a player.
func offlineUUID(name string) string {
	sum := md5.Sum([]byte("OfflinePlayer:" + name))
	sum[6] = (sum[6] & 0x0f) | 0x30 // RFC 4122 v3
	sum[8] = (sum[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// Backup flushes world data and archives the server directory. The server may
// be running: "save-all" is issued first so the archive is consistent.
func (in *Instance) Backup(ctx context.Context) (string, error) {
	if in.IsRunning() {
		_ = in.Say("manager: creating a backup, expect a short lag spike")
		if _, err := in.exec("save-all flush"); err != nil {
			in.log.Writef("[Manager] save-all failed, backing up anyway: %v", err)
		}
		time.Sleep(2 * time.Second)
	}

	dest := backupDir(in)
	name := fmt.Sprintf("%s-%s", in.cfg.ID, time.Now().Format("2006-01-02_15-04-05"))
	in.recordEvent("backup", "creating backup %s", name)
	info, err := backup.Create(ctx, in.dir, dest, name)
	if err != nil {
		in.recordEvent("backup", "backup failed: %v", err)
		return "", err
	}
	if err := backup.Prune(dest, in.app.BackupsToKeep); err != nil {
		in.log.Writef("[Manager] prune failed: %v", err)
	}
	in.recordEvent("backup", "backup complete: %s (%s)", info.Name, humanBytes(info.Size))
	return info.Name, nil
}

// Backups lists the archives kept for this instance.
func (in *Instance) Backups() []backup.Info {
	list, err := backup.List(filepath.Join(in.app.DataDir, "backups", in.cfg.ID))
	if err != nil {
		return nil
	}
	return list
}

// RestoreBackup replaces the server files with an archive. The server must be
// stopped: restoring over a running world would corrupt it.
func (in *Instance) RestoreBackup(ctx context.Context, name string) error {
	if !in.State().Terminal() {
		return ErrAlreadyRunning
	}
	if !strings.HasSuffix(name, ".zip") {
		name += ".zip"
	}
	src := filepath.Join(in.app.DataDir, "backups", in.cfg.ID, name)
	if _, err := os.Stat(src); err != nil {
		return err
	}
	in.recordEvent("backup", "restoring %s", name)
	return backup.Restore(ctx, src, in.dir)
}
