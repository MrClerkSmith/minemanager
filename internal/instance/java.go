// Java runtime provisioning: when none of the installed JVMs fits a server's
// requirements (Forge in particular bundles an ASM version that cannot read
// newer class files, so it must not run on a too-new JDK), the manager
// downloads an Eclipse Temurin JDK into <data>/java and uses it.
package instance

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// isWindows / runtimeArch / archiveExt are shared with the instance package's
// other JVM helpers.
var isWindows = runtime.GOOS == "windows"

const runtimeArch = runtime.GOARCH

func archiveExt() string {
	if isWindows {
		return ".zip"
	}
	return ".tar.gz"
}

// provisionJVM downloads a Temurin JDK of the given major version into
// <data>/java and returns the path to its java binary. An already provisioned
// distribution is reused without contacting the network.
func (in *Instance) provisionJVM(major int) (string, error) {
	root := filepath.Join(in.app.DataDir, "java")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	if p, ok := findJavaIn(root); ok {
		return p, nil
	}

	goos := "linux"
	if isWindows {
		goos = "windows"
	}
	arch := "x64"
	if !strings.HasSuffix(runtimeArch, "amd64") {
		arch = "aarch64"
	}
	url := fmt.Sprintf("https://api.adoptium.net/v3/binary/latest/%d/ga/%s/%s/jdk/hotspot/normal/eclipse",
		major, goos, arch)

	in.log.Writef("[Manager] no suitable local JVM, provisioning Java %d from Adoptium", major)
	archivePath := filepath.Join(root, fmt.Sprintf("jdk-%d%s", major, archiveExt()))
	if err := in.download(context.Background(), url, archivePath); err != nil {
		return "", err
	}
	defer os.Remove(archivePath)

	staging := filepath.Join(root, fmt.Sprintf("jdk-%d.staging", major))
	_ = os.RemoveAll(staging)
	if err := extractArchive(archivePath, staging); err != nil {
		_ = os.RemoveAll(staging)
		return "", err
	}

	// The archive holds one top-level directory; promote it to a stable name.
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) == 0 {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("downloaded jdk archive is empty")
	}
	final := filepath.Join(root, fmt.Sprintf("jdk-%d", major))
	tmp := final + ".old"
	_ = os.RemoveAll(tmp)
	_ = os.Rename(final, tmp)
	if err := os.Rename(filepath.Join(staging, entries[0].Name()), final); err != nil {
		_ = os.Rename(tmp, final)
		return "", err
	}
	_ = os.RemoveAll(staging)
	_ = os.RemoveAll(tmp)

	p, ok := findJavaIn(final)
	if !ok {
		return "", fmt.Errorf("provisioned jdk %d has no java binary", major)
	}
	in.recordEvent("install", "provisioned Java %d into %s", major, final)
	return p, nil
}

// findJavaIn locates bin/java (bin/java.exe on Windows) anywhere under dir.
func findJavaIn(dir string) (string, bool) {
	var found string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == "java" || name == "java.exe" {
			if filepath.Base(filepath.Dir(path)) == "bin" {
				found = path
				return io.EOF // stop walking
			}
		}
		return nil
	})
	return found, found != ""
}

// extractArchive unpacks the downloaded JDK archive into dst.
func extractArchive(src, dst string) error {
	if isWindows {
		return extractZip(src, dst)
	}
	return extractTarGz(src, dst)
}

func extractZip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open jdk archive: %w", err)
	}
	defer r.Close()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, f := range r.File {
		target := filepath.Join(dst, filepath.FromSlash(f.Name))
		// Guard against zip-slip.
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry outside target dir: %s", f.Name)
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
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			out.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTarGz(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open jdk archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read jdk archive: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry outside target dir: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		case tar.TypeSymlink:
			// JDK archives contain relative symlinks; keep them as-is.
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				// Some filesystems disallow symlinks; the JDK still works
				// without the few optional ones it ships.
				continue
			}
		}
	}
}
