// Package backup creates, lists, prunes and restores zip archives of a server
// directory. Archives exclude volatile caches and the manager's own logs so
// they stay small enough to keep several around.
package backup

import (
	"archive/zip"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Info describes one archive on disk.
type Info struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
	Path string    `json:"-"`
}

// excludeNames are directories / files never included in an archive.
var excludeNames = map[string]bool{
	"logs":         true,
	"backups":      true,
	"cache":        true,
	".cache":       true,
	"tmp":          true,
	"libraries":    true, // forge: re-installable, hundreds of MB
	".gradle":      true,
	"session.lock": true,
}

// excluded reports whether a file system entry must be skipped.
func excluded(name string, isDir bool) bool {
	if excludeNames[name] {
		return true
	}
	if !isDir {
		switch {
		case strings.HasSuffix(name, ".part"), strings.HasSuffix(name, ".tmp"):
			return true
		}
	}
	return false
}

// Create writes a zip archive of dir into destDir and returns its info.
func Create(ctx context.Context, dir, destDir, name string) (Info, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Info{}, err
	}
	if !strings.HasSuffix(name, ".zip") {
		name += ".zip"
	}
	dest := filepath.Join(destDir, name)

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return Info{}, err
	}
	// Remove the partial file if archiving fails mid-way.
	defer func() { _ = os.Remove(tmp) }()

	zw := zip.NewWriter(f)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil || rel == "." {
			return nil
		}
		if excluded(d.Name(), d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, src)
		src.Close()
		return err
	})
	if err != nil {
		zw.Close()
		f.Close()
		return Info{}, err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return Info{}, err
	}
	if err := f.Close(); err != nil {
		return Info{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return Info{}, err
	}
	st, err := os.Stat(dest)
	if err != nil {
		return Info{}, err
	}
	return Info{Name: name, Size: st.Size(), Time: st.ModTime(), Path: dest}, nil
}

// List returns the archives in dir, newest first.
func List(dir string) ([]Info, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".zip") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{
			Name: e.Name(),
			Size: fi.Size(),
			Time: fi.ModTime(),
			Path: filepath.Join(dir, e.Name()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// Prune keeps at most keep archives, deleting the oldest ones.
func Prune(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	list, err := List(dir)
	if err != nil || len(list) <= keep {
		return err
	}
	for _, old := range list[keep:] {
		_ = os.Remove(old.Path)
	}
	return nil
}

// Restore extracts an archive over dir. Existing files are overwritten.
func Restore(ctx context.Context, src, dir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		target := filepath.Join(dir, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) &&
			target != filepath.Clean(dir) {
			return &fs.PathError{Op: "restore", Path: f.Name, Err: os.ErrPermission}
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		rc.Close()
		out.Close()
		if copyErr != nil {
			return copyErr
		}
	}
	return nil
}
