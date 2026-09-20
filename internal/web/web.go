// Package web serves the management REST API and the embedded Web UI.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"minemanager/internal/config"
	"minemanager/internal/instance"
	"minemanager/internal/logger"
	"minemanager/internal/manager"
)

//go:embed static
var staticFS embed.FS

// Handler implements http.Handler for the whole management surface.
type Handler struct {
	mgr *manager.Manager
	log *slog.Logger
	mux *http.ServeMux
}

// New wires every route and returns the handler.
func New(mgr *manager.Manager) *Handler {
	h := &Handler{
		mgr: mgr,
		log: slog.Default().With("component", "web"),
	}
	mux := http.NewServeMux()

	// Web UI
	mux.HandleFunc("GET /", h.index)
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		h.log.Error("static asset embed broken", "err", err)
	} else {
		mux.Handle("GET /static/", http.StripPrefix("/static/",
			http.FileServer(http.FS(staticSub))))
	}

	// Metadata
	mux.HandleFunc("GET /api/config", h.appConfig)
	mux.HandleFunc("GET /api/types", h.types)
	mux.HandleFunc("GET /api/versions", h.versions)

	// Servers
	mux.HandleFunc("GET /api/servers", h.listServers)
	mux.HandleFunc("POST /api/servers", h.createServer)
	mux.HandleFunc("GET /api/servers/{id}", h.getServer)
	mux.HandleFunc("PUT /api/servers/{id}", h.updateServer)
	mux.HandleFunc("DELETE /api/servers/{id}", h.deleteServer)
	mux.HandleFunc("GET /api/servers/{id}/icon", h.getIcon)
	mux.HandleFunc("POST /api/servers/{id}/icon", h.uploadIcon)
	mux.HandleFunc("DELETE /api/servers/{id}/icon", h.deleteIcon)
	mux.HandleFunc("POST /api/servers/{id}/{action}", h.serverAction)

	// Console
	mux.HandleFunc("GET /api/servers/{id}/console", h.consoleSSE)
	mux.HandleFunc("GET /api/servers/{id}/console/history", h.consoleHistory)
	mux.HandleFunc("POST /api/servers/{id}/command", h.sendCommand)

	// Players & whitelist
	mux.HandleFunc("GET /api/servers/{id}/players", h.players)
	mux.HandleFunc("POST /api/servers/{id}/players/{name}/{action}", h.playerAction)
	mux.HandleFunc("GET /api/servers/{id}/whitelist", h.whitelistList)
	mux.HandleFunc("POST /api/servers/{id}/whitelist", h.whitelistAction)

	// Backups
	mux.HandleFunc("GET /api/servers/{id}/backups", h.backups)
	mux.HandleFunc("GET /api/servers/{id}/backups/{name}", h.downloadBackup)
	mux.HandleFunc("POST /api/servers/{id}/backups/{name}/restore", h.restoreBackup)

	// Deployment exports
	mux.HandleFunc("GET /api/servers/{id}/export/{format}", h.export)

	h.mux = mux
	return h
}

// ServeHTTP dispatches to the configured mux.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// index serves the single page application.
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "index.html is missing")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// appConfig returns manager settings.
func (h *Handler) appConfig(w http.ResponseWriter, r *http.Request) {
	c := h.mgr.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"data_dir":        c.DataDir,
		"host":            c.Host,
		"default_java":    c.DefaultJava,
		"backups_to_keep": c.BackupsToKeep,
		"max_upload_mb":   c.MaxUploadMB,
	})
}

// types lists the supported server softwares.
func (h *Handler) types(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]string, 0)
	for _, t := range config.KnownTypes() {
		out = append(out, map[string]string{
			"id":    string(t),
			"label": t.DisplayName(),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// versions enumerates game versions or, with ?game=, builds/loader versions.
func (h *Handler) versions(w http.ResponseWriter, r *http.Request) {
	typ := config.ServerType(r.URL.Query().Get("type"))
	game := r.URL.Query().Get("game")
	if !typ.IsValid() {
		writeError(w, http.StatusBadRequest, "unsupported type")
		return
	}
	if game == "" {
		v, err := h.mgr.Versions().Versions(r.Context(), typ)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": v})
		return
	}
	builds, err := h.mgr.Versions().Builds(r.Context(), typ, game)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": builds})
}

// listServers returns the status of every server.
func (h *Handler) listServers(w http.ResponseWriter, r *http.Request) {
	out := make([]instance.Status, 0)
	for _, in := range h.mgr.List() {
		out = append(out, in.Status())
	}
	writeJSON(w, http.StatusOK, out)
}

// getServer returns the status of one server.
func (h *Handler) getServer(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	writeJSON(w, http.StatusOK, in.Status())
}

// createServer provisions a new instance.
func (h *Handler) createServer(w http.ResponseWriter, r *http.Request) {
	var req manager.CreateRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in, err := h.mgr.Create(r.Context(), &req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, in.Status())
}

// updateServer applies settings changes.
func (h *Handler) updateServer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var s manager.Settings
	if err := decodeJSON(r, &s); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.mgr.ApplySettings(r.Context(), id, &s); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in, _ := h.mgr.Get(id)
	writeJSON(w, http.StatusOK, in.Status())
}

// deleteServer removes an instance, including its files unless ?keep=true.
func (h *Handler) deleteServer(w http.ResponseWriter, r *http.Request) {
	keep := r.URL.Query().Has("keep")
	if err := h.mgr.Delete(r.Context(), r.PathValue("id"), keep); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// getIcon serves a custom icon image uploaded for a server.
func (h *Handler) getIcon(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	path, ok := in.IconFile()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", iconContentType(path))
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, path)
}

// uploadIcon stores a custom icon image for a server, replacing any previous one.
func (h *Handler) uploadIcon(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	limit := int64(h.mgr.Config().MaxUploadMB) << 20
	if err := r.ParseMultipartForm(limit); err != nil {
		writeError(w, http.StatusBadRequest, "icon too large or invalid upload: "+err.Error())
		return
	}
	file, header, err := r.FormFile("icon")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'icon' file field")
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp":
	default:
		writeError(w, http.StatusBadRequest,
			"icon must be an image (png, jpg, gif, webp, svg, bmp)")
		return
	}
	// Sniff the payload: SVG starts as XML, everything else must smell like an
	// image to the content-type detector.
	if ext != ".svg" {
		sniff := make([]byte, 512)
		n, _ := io.ReadFull(file, sniff)
		if ct := http.DetectContentType(sniff[:n]); !strings.HasPrefix(ct, "image/") {
			writeError(w, http.StatusBadRequest, "uploaded file is not a valid image")
			return
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if err := os.MkdirAll(in.Dir(), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := in.RemoveIcon(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	target := filepath.Join(in.Dir(), "icon"+ext)
	out, err := os.Create(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		_ = os.Remove(target)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out.Close()
	writeJSON(w, http.StatusOK, in.Status())
}

// deleteIcon removes a custom icon image.
func (h *Handler) deleteIcon(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if err := in.RemoveIcon(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// iconContentType resolves the mime type for an icon file, svg included.
func iconContentType(path string) string {
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// serverAction runs a lifecycle action asynchronously.
func (h *Handler) serverAction(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	action := r.PathValue("action")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	switch action {
	case "start":
		go func() { defer cancel(); _ = in.Start(ctx) }()
	case "stop":
		go func() { defer cancel(); _ = in.Stop(ctx) }()
	case "restart":
		go func() { defer cancel(); _ = in.Restart(ctx) }()
	case "kill":
		go func() { defer cancel(); _ = in.Kill() }()
	case "install":
		go func() { defer cancel(); _ = in.Install(ctx) }()
	case "update":
		go func() { defer cancel(); _ = in.Update(ctx) }()
	case "backup":
		go func() {
			defer cancel()
			if _, err := in.Backup(ctx); err != nil {
				h.log.Warn("backup failed", "server", in.ID(), "err", err)
			}
		}()
	default:
		cancel()
		writeError(w, http.StatusNotFound, "unknown action "+action)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "action": action})
}

// consoleHistory returns the buffered console lines.
func (h *Handler) consoleHistory(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	writeJSON(w, http.StatusOK, in.Logger().History(500))
}

// consoleSSE streams live console lines to the browser.
func (h *Handler) consoleSSE(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, hist := in.Logger().Subscribe()
	defer in.Logger().Unsubscribe(ch)
	for _, line := range hist {
		writeSSE(w, line)
	}
	flusher.Flush()

	notify := r.Context().Done()
	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, line)
			flusher.Flush()
		case <-notify:
			return
		}
	}
}

// sendCommand types into the server console.
func (h *Handler) sendCommand(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	var body struct {
		Command string `json:"command"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Command == "" {
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	if err := in.Command(body.Command); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sent": true})
}

// players lists online players.
func (h *Handler) players(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	players, err := in.Players()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"online":  0,
			"players": []any{},
			"error":   err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"online":  len(players),
		"players": players,
	})
}

// playerAction kicks or bans a player.
func (h *Handler) playerAction(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	name := r.PathValue("name")
	action := r.PathValue("action")
	var err error
	switch action {
	case "kick":
		err = in.Kick(name)
	case "ban":
		err = in.Ban(name)
	case "pardon":
		err = in.Pardon(name)
	default:
		writeError(w, http.StatusNotFound, "unknown player action "+action)
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// whitelistList returns the whitelist entries.
func (h *Handler) whitelistList(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": in.Cfg().Whitelist,
		"entries": in.WhitelistList(),
	})
}

// whitelistAction adds/removes players or toggles the whitelist.
func (h *Handler) whitelistAction(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	var body struct {
		Action string `json:"action"`
		Player string `json:"player"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	action := instance.WhitelistAction(body.Action)
	switch action {
	case instance.WLAdd, instance.WLRemove, instance.WLEnable, instance.WLDisable:
	default:
		writeError(w, http.StatusBadRequest, "unknown whitelist action "+body.Action)
		return
	}
	if err := in.Whitelist(action, body.Player); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// backups lists the archives of a server.
func (h *Handler) backups(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	writeJSON(w, http.StatusOK, in.Backups())
}

// downloadBackup serves one archive.
func (h *Handler) downloadBackup(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	name := r.PathValue("name")
	for _, b := range in.Backups() {
		if b.Name == name {
			w.Header().Set("Content-Disposition",
				"attachment; filename=\""+name+"\"")
			http.ServeFile(w, r, b.Path)
			return
		}
	}
	writeError(w, http.StatusNotFound, "backup not found")
}

// restoreBackup replaces the server files with an archive.
func (h *Handler) restoreBackup(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if err := in.RestoreBackup(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": true})
}

// export returns a systemd unit or docker-compose.yml.
func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	in, ok := h.mgr.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	var content, fileName string
	switch r.PathValue("format") {
	case "systemd":
		content = in.ExportSystemd()
		fileName = in.ID() + ".service"
	case "docker", "compose":
		content = in.ExportDockerCompose()
		fileName = "docker-compose.yml"
	default:
		writeError(w, http.StatusNotFound, "unknown export format")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=\""+fileName+"\"")
	_, _ = w.Write([]byte(content))
}

// writeSSE emits one console line as a Server-Sent Event.
func writeSSE(w http.ResponseWriter, line logger.Line) {
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}

// decodeJSON reads a request body into v, rejecting unknown fields.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// writeJSON serializes v as a JSON response.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error envelope.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}
