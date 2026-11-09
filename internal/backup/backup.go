// Package backup makes and restores one verified tarball of Chhaya's data:
// ~/.chhaya/hermes (the agent's home) and ~/.chhaya/gateway (audit log, kill switch,
// Personal memory). It runs from the host, where both directories are visible, and
// deliberately not inside the gateway daemon: the daemon cannot see Hermes' home, and
// restore has to work when nothing is running. See docs/decisions.md, 2026-11-09.
//
// The caller stops the stack first. A cold copy is exactly consistent, which is why this
// package needs no SQLite library: databases are copied as files and checked with the
// sqlite3 CLI.
//
// Every archive carries MANIFEST.json (the last entry) listing every file with its size,
// mode and SHA-256, plus a .sha256 sidecar of the whole archive. Restore needs both: the
// sidecar catches damage in transit, the manifest catches an archive someone rebuilt with a
// fixed-up sidecar. Restore never overwrites, extracts to a temporary directory, checks
// everything, and only then moves the result into place.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
)

const (
	manifestName = "MANIFEST.json"
	statusFile   = "backups/last-success.json"
	maxManifest  = 64 << 20
)

var tops = []string{"hermes", "gateway"}

// Checker validates one SQLite database file. nil means it is sound.
type Checker func(dbPath string) error

// Runner runs an external command (the aws CLI). Injected so tests need no network.
type Runner func(name string, args ...string) error

type Options struct {
	Home    string // ~/.chhaya
	OutDir  string
	Now     time.Time
	Host    string
	CheckDB Checker
}

type fileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Version int         `json:"version"`
	Created time.Time   `json:"created"`
	Host    string      `json:"host"`
	Dirs    []string    `json:"dirs"`
	Files   []fileEntry `json:"files"`
}

// excluded: lock files carry a pid that means nothing on another machine, and sockets
// are not data.
func excluded(name string, mode fs.FileMode) bool {
	return mode&fs.ModeSocket != 0 || (mode.IsRegular() && (strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".sock")))
}

// Create archives Home into OutDir and returns the archive path.
func Create(o Options) (string, error) {
	for _, d := range tops {
		fi, err := os.Stat(filepath.Join(o.Home, d))
		if err != nil || !fi.IsDir() {
			return "", fmt.Errorf("no %s directory under %s", d, o.Home)
		}
	}
	if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
		return "", err
	}

	type item struct {
		rel  string
		info fs.FileInfo
	}
	var items []item
	for _, top := range tops {
		err := filepath.WalkDir(filepath.Join(o.Home, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(o.Home, p)
			rel = filepath.ToSlash(rel)
			switch {
			case excluded(d.Name(), info.Mode()):
				return nil
			case info.IsDir(), info.Mode().IsRegular():
				items = append(items, item{rel, info})
				return nil
			default:
				return fmt.Errorf("%s is a %s; only files and directories are backed up", rel, kind(info.Mode()))
			}
		})
		if err != nil {
			return "", err
		}
	}

	// Check the source before spending time archiving it.
	if o.CheckDB != nil {
		for _, it := range items {
			if it.info.Mode().IsRegular() && strings.HasSuffix(it.rel, ".db") {
				if err := o.CheckDB(filepath.Join(o.Home, it.rel)); err != nil {
					return "", fmt.Errorf("%s failed its integrity check: %w", it.rel, err)
				}
			}
		}
	}
	if err := verifyAudit(filepath.Join(o.Home, "gateway")); err != nil {
		return "", err
	}

	name := fmt.Sprintf("chhaya-data-%s-%s.tar.gz", o.Host, o.Now.UTC().Format("20060102-150405"))
	final := filepath.Join(o.OutDir, name)
	tmp := final + ".partial"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	cleanup := func() { f.Close(); os.Remove(tmp) }
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	m := manifest{Version: 1, Created: o.Now.UTC(), Host: o.Host}
	for _, it := range items {
		h := &tar.Header{Name: it.rel, ModTime: it.info.ModTime(), Mode: int64(it.info.Mode().Perm())}
		if it.info.IsDir() {
			h.Typeflag = tar.TypeDir
			h.Name += "/"
			if err := tw.WriteHeader(h); err != nil {
				cleanup()
				return "", err
			}
			m.Dirs = append(m.Dirs, it.rel)
			continue
		}
		sum, n, err := writeFile(tw, h, filepath.Join(o.Home, it.rel), it.info.Size())
		if err != nil {
			cleanup()
			return "", fmt.Errorf("%s: %w", it.rel, err)
		}
		m.Files = append(m.Files, fileEntry{Path: it.rel, Size: n, Mode: uint32(it.info.Mode().Perm()), SHA256: sum})
	}
	mb, _ := json.MarshalIndent(m, "", " ")
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mb)), Typeflag: tar.TypeReg, ModTime: o.Now}); err != nil {
		cleanup()
		return "", err
	}
	tw.Write(mb)
	if err := tw.Close(); err != nil {
		cleanup()
		return "", err
	}
	if err := gw.Close(); err != nil {
		cleanup()
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", err
	}
	sum, err := fileSHA256(final)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(final+".sha256", []byte(sum+"  "+name+"\n"), 0o600); err != nil {
		return "", err
	}
	// Read it back as restore would. A backup that cannot be read is not a backup.
	if err := Verify(final); err != nil {
		return "", fmt.Errorf("the archive we just wrote does not verify: %w", err)
	}
	return final, nil
}

func writeFile(tw *tar.Writer, h *tar.Header, path string, size int64) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h.Typeflag = tar.TypeReg
	h.Size = size
	if err := tw.WriteHeader(h); err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	n, err := io.CopyN(io.MultiWriter(tw, hash), f, size)
	if err != nil {
		return "", n, fmt.Errorf("file changed while being read (is the stack stopped?): %w", err)
	}
	if extra, _ := io.CopyN(io.Discard, f, 1); extra > 0 {
		return "", n, errors.New("file grew while being read (is the stack stopped?)")
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

func kind(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

func verifyAudit(gatewayDir string) error {
	p := filepath.Join(gatewayDir, "audit.jsonl")
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := audit.Verify(p); err != nil {
		return fmt.Errorf("audit chain does not verify: %w", err)
	}
	return nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkSidecar(archive string) error {
	b, err := os.ReadFile(archive + ".sha256")
	if err != nil {
		return fmt.Errorf("no checksum file beside the archive: %w", err)
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return errors.New("empty checksum file")
	}
	got, err := fileSHA256(archive)
	if err != nil {
		return err
	}
	if !strings.EqualFold(fields[0], got) {
		return errors.New("checksum mismatch: the archive is damaged or was changed")
	}
	return nil
}

// Verify checks the sidecar checksum and every file against the manifest, without
// extracting anything.
func Verify(archive string) error {
	if err := checkSidecar(archive); err != nil {
		return err
	}
	_, err := extract(archive, "")
	return err
}

// extract walks the archive. With dest set it also writes the files under dest. It
// refuses any entry that is not a regular file or directory directly inside hermes/ or
// gateway/, and returns an error unless the archive matches its manifest exactly.
func extract(archive, dest string) (*manifest, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)

	seenDirs := map[string]bool{}
	seenFiles := map[string]fileEntry{}
	var m *manifest
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if m != nil {
			return nil, errors.New("entries after the manifest")
		}
		if h.Name == manifestName && h.Typeflag == tar.TypeReg {
			b, err := io.ReadAll(io.LimitReader(tr, maxManifest))
			if err != nil {
				return nil, err
			}
			m = &manifest{}
			if err := json.Unmarshal(b, m); err != nil {
				return nil, fmt.Errorf("unreadable manifest: %w", err)
			}
			continue
		}
		name := strings.TrimSuffix(h.Name, "/")
		if err := safeName(name); err != nil {
			return nil, err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if seenDirs[name] {
				return nil, fmt.Errorf("duplicate entry %s", name)
			}
			seenDirs[name] = true
			if dest != "" {
				mode := fs.FileMode(h.Mode&0o777) | 0o700
				if !strings.Contains(name, "/") {
					mode = 0o700
				}
				if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(name)), mode); err != nil {
					return nil, err
				}
				os.Chmod(filepath.Join(dest, filepath.FromSlash(name)), mode)
			}
		case tar.TypeReg:
			if _, dup := seenFiles[name]; dup {
				return nil, fmt.Errorf("duplicate entry %s", name)
			}
			sum, err := copyEntry(tr, h, dest, name)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			seenFiles[name] = fileEntry{Path: name, Size: h.Size, Mode: uint32(h.Mode & 0o777), SHA256: sum}
		default:
			return nil, fmt.Errorf("%s: entry type %q is not allowed in a Chhaya backup", name, h.Typeflag)
		}
	}
	if m == nil {
		return nil, errors.New("archive has no manifest")
	}
	if len(m.Files) != len(seenFiles) || len(m.Dirs) != len(seenDirs) {
		return nil, fmt.Errorf("archive does not match its manifest: %d files and %d directories listed, %d and %d present", len(m.Files), len(m.Dirs), len(seenFiles), len(seenDirs))
	}
	for _, d := range m.Dirs {
		if !seenDirs[d] {
			return nil, fmt.Errorf("directory %s is in the manifest but not the archive", d)
		}
	}
	for _, want := range m.Files {
		got, ok := seenFiles[want.Path]
		switch {
		case !ok:
			return nil, fmt.Errorf("%s is in the manifest but not the archive", want.Path)
		case got.SHA256 != want.SHA256 || got.Size != want.Size:
			return nil, fmt.Errorf("%s does not match the manifest (changed after the backup was taken)", want.Path)
		case got.Mode != want.Mode:
			return nil, fmt.Errorf("%s has mode %o, manifest says %o", want.Path, got.Mode, want.Mode)
		}
	}
	return m, nil
}

func safeName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name || strings.Contains(name, "\\") {
		return fmt.Errorf("unsafe path %q in archive", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "" {
			return fmt.Errorf("unsafe path %q in archive", name)
		}
	}
	top, _, _ := strings.Cut(name, "/")
	if top != "hermes" && top != "gateway" {
		return fmt.Errorf("path %q is outside hermes/ and gateway/", name)
	}
	return nil
}

func copyEntry(r io.Reader, h *tar.Header, dest, name string) (string, error) {
	hash := sha256.New()
	if dest == "" {
		n, err := io.Copy(hash, r)
		if err != nil || n != h.Size {
			return "", fmt.Errorf("short read: %v", err)
		}
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	p := filepath.Join(dest, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(io.MultiWriter(out, hash), r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil || n != h.Size {
		return "", fmt.Errorf("short write: %v", err)
	}
	if err := os.Chmod(p, fs.FileMode(h.Mode&0o777)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Restore puts the archive's data into home/hermes and home/gateway. It refuses if either
// already holds anything, and leaves nothing behind if any check fails.
func Restore(archive, home string, check Checker) error {
	if err := checkSidecar(archive); err != nil {
		return err
	}
	for _, d := range tops {
		es, err := os.ReadDir(filepath.Join(home, d))
		if err == nil && len(es) > 0 {
			return fmt.Errorf("%s is not empty; restore never overwrites", filepath.Join(home, d))
		}
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(home, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	if _, err := extract(archive, tmp); err != nil {
		return err
	}
	if check != nil {
		var dbs []string
		filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && strings.HasSuffix(p, ".db") {
				dbs = append(dbs, p)
			}
			return nil
		})
		sort.Strings(dbs)
		for _, p := range dbs {
			if err := check(p); err != nil {
				rel, _ := filepath.Rel(tmp, p)
				return fmt.Errorf("restored %s failed its integrity check: %w", rel, err)
			}
		}
	}
	if err := verifyAudit(filepath.Join(tmp, "gateway")); err != nil {
		return err
	}

	var moved []string
	for _, d := range tops {
		dst := filepath.Join(home, d)
		os.Remove(dst) // an empty directory is not data
		if err := os.Rename(filepath.Join(tmp, d), dst); err != nil {
			for _, m := range moved {
				os.Rename(filepath.Join(home, m), filepath.Join(tmp, m))
			}
			return fmt.Errorf("moving %s into place: %w", d, err)
		}
		moved = append(moved, d)
	}
	return nil
}

// Push uploads the archive and its checksum, and only then records the success that
// Status looks for. It refuses an archive that does not verify.
func Push(home, archive, s3uri string, run Runner, now time.Time) error {
	if err := Verify(archive); err != nil {
		return fmt.Errorf("refusing to upload: %w", err)
	}
	dst := strings.TrimRight(s3uri, "/") + "/" + filepath.Base(archive)
	for _, pair := range [][2]string{{archive, dst}, {archive + ".sha256", dst + ".sha256"}} {
		if err := run("aws", "s3", "cp", "--only-show-errors", pair[0], pair[1]); err != nil {
			return fmt.Errorf("upload failed: %w", err)
		}
	}
	rec, _ := json.Marshal(map[string]any{"time": now.UTC(), "archive": filepath.Base(archive), "s3": dst})
	p := filepath.Join(home, statusFile)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, rec, 0o600)
}

// Pull downloads an archive and its checksum into dir. s3uri names the archive.
func Pull(s3uri, dir string, run Runner) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	base := path.Base(s3uri)
	for _, suffix := range []string{"", ".sha256"} {
		if err := run("aws", "s3", "cp", "--only-show-errors", s3uri+suffix, filepath.Join(dir, base+suffix)); err != nil {
			return fmt.Errorf("download failed: %w", err)
		}
	}
	return nil
}

// Status fails unless a backup was uploaded within maxAge of now.
func Status(home string, maxAge time.Duration, now time.Time) error {
	b, err := os.ReadFile(filepath.Join(home, statusFile))
	if err != nil {
		return errors.New("no successful backup has ever been recorded")
	}
	var rec struct {
		Time    time.Time `json:"time"`
		Archive string    `json:"archive"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return fmt.Errorf("unreadable backup status: %w", err)
	}
	if age := now.Sub(rec.Time); age > maxAge {
		return fmt.Errorf("last successful backup was %s ago (%s), limit %s", age.Round(time.Minute), rec.Archive, maxAge)
	}
	return nil
}

// SQLiteChecker runs PRAGMA integrity_check through the sqlite3 CLI. Fail closed: if the
// CLI is missing the check is an error, not a skip.
func SQLiteChecker() Checker {
	return func(p string) error {
		bin, err := exec.LookPath("sqlite3")
		if err != nil {
			return errors.New("sqlite3 is not installed (apt install sqlite3); refusing to skip the integrity check")
		}
		out, err := exec.Command(bin, "-readonly", p, "PRAGMA integrity_check;").CombinedOutput()
		if err != nil {
			return fmt.Errorf("sqlite3: %v: %s", err, strings.TrimSpace(string(out)))
		}
		if s := strings.TrimSpace(string(out)); s != "ok" {
			return fmt.Errorf("integrity_check said %q", s)
		}
		return nil
	}
}
