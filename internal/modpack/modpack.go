// Package modpack installs modpack archives into a server instance directory.
//
// Two archive shapes are understood:
//
//   - Modrinth packs: a zip containing modrinth.index.json plus an overrides/
//     directory. Every mod listed in the index is downloaded from its CDN url.
//   - "overrides" packs (incl. most CurseForge exports): a zip whose overrides/
//     directory holds the mods/ and config/ trees; mods are copied as-is.
package modpack

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Installer places a downloaded modpack into a server directory.
type Installer struct {
	http *http.Client
}

// New returns an installer ready to download from the public mod CDNs.
func New() *Installer {
	return &Installer{http: &http.Client{Timeout: 0}} // downloads may be large
}

// indexFile is the subset of modrinth.index.json we care about.
type indexFile struct {
	FormatVersion int    `json:"formatVersion"`
	Game          string `json:"game"`
	VersionID     string `json:"versionId"`
	Name          string `json:"name"`
	Files         []struct {
		Path      string            `json:"path"`
		Hashes    map[string]string `json:"hashes"`
		FileSize  int64             `json:"fileSize"`
		Downloads []string          `json:"downloads"`
		Env       map[string]string `json:"env"`
	} `json:"files"`
}

// Install downloads src (a URL or local path to a .zip) and merges it into dir.
// It returns a human readable summary of what was installed.
func (in *Installer) Install(ctx context.Context, src, dir string, log func(string, ...any)) (string, error) {
	zipPath, cleanup, err := in.fetch(ctx, src)
	if err != nil {
		return "", err
	}
	defer cleanup()

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("open modpack zip: %w", err)
	}
	defer zr.Close()

	var idx *indexFile
	if f, err := zr.Open("modrinth.index.json"); err == nil {
		data, _ := io.ReadAll(f)
		f.Close()
		var parsed indexFile
		if err := json.Unmarshal(data, &parsed); err == nil && parsed.Game == "minecraft" {
			idx = &parsed
		}
	}

	modsDir := filepath.Join(dir, "mods")
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		return "", err
	}

	// 1. Extract the overrides tree (configs, mods shipped inside the pack).
	extracted := 0
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "overrides/") {
			continue
		}
		rel := strings.TrimPrefix(f.Name, "overrides/")
		if rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		// Guard against zip-slip.
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) && target != filepath.Clean(dir) {
			return "", fmt.Errorf("modpack zip contains unsafe path %q", f.Name)
		}
		if err := extractFile(f, target); err != nil {
			log("modpack: skip %s: %v", f.Name, err)
			continue
		}
		extracted++
	}
	log("modpack: extracted %d override files", extracted)

	// 2. Download mods declared in a Modrinth index.
	downloaded := 0
	if idx != nil {
		log("modpack: %s (%s), %d declared files", idx.Name, idx.VersionID, len(idx.Files))
		for _, mf := range idx.Files {
			if mf.Env != nil {
				// Skip client-only mods; a dedicated server does not need them.
				if side, ok := mf.Env["client"]; ok && side == "required" {
					if server, ok := mf.Env["server"]; ok && server == "unsupported" {
						continue
					}
				}
			}
			if !strings.HasPrefix(mf.Path, "mods/") {
				continue
			}
			target := filepath.Join(dir, filepath.FromSlash(mf.Path))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			if _, err := os.Stat(target); err == nil {
				downloaded++
				continue // already present
			}
			if err := in.downloadFile(ctx, mf.Downloads, target); err != nil {
				log("modpack: failed to download %s: %v", mf.Path, err)
				continue
			}
			downloaded++
		}
	}

	switch {
	case idx != nil:
		return fmt.Sprintf("installed modrinth pack %q (%s): %d mods", idx.Name, idx.VersionID, downloaded), nil
	default:
		return fmt.Sprintf("installed modpack: %d files extracted into overrides/", extracted), nil
	}
}

func (in *Installer) fetch(ctx context.Context, src string) (path string, cleanup func(), err error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return "", nil, err
		}
		req.Header.Set("User-Agent", "MinecraftManager/1.0")
		resp, err := in.http.Do(req)
		if err != nil {
			return "", nil, fmt.Errorf("download modpack: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", nil, fmt.Errorf("download modpack: %s", resp.Status)
		}
		tmp, err := os.CreateTemp("", "modpack-*.zip")
		if err != nil {
			return "", nil, err
		}
		if _, err := io.Copy(tmp, resp.Body); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", nil, err
		}
		tmp.Close()
		return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
	}
	if _, err := os.Stat(src); err != nil {
		return "", nil, fmt.Errorf("modpack source: %w", err)
	}
	return src, func() {}, nil
}

func extractFile(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	_, err = io.Copy(out, rc)
	return err
}

func (in *Installer) downloadFile(ctx context.Context, urls []string, target string) error {
	var lastErr error
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "MinecraftManager/1.0 (+server-manager)")
		resp, err := in.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: %s", u, resp.Status)
			continue
		}
		out, err := os.Create(target)
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, err = io.Copy(out, resp.Body)
		out.Close()
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}
