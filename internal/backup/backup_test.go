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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/satishthakur/chhaya/internal/audit"
)

var t0 = time.Date(2026, 11, 10, 20, 30, 0, 0, time.UTC)

func okCheck(string) error { return nil }

// seed builds a small ~/.chhaya: a Hermes home, a gateway home with a real audit chain,
// and a nested git-style directory with dotfiles, the shapes the real data has.
func seed(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".chhaya")
	w := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	w("hermes/state.db", "SQLite format 3\x00 pretend rows", 0o644)
	w("hermes/state.db-wal", "wal bytes", 0o644)
	w("hermes/.env", "SECRET=1\n", 0o600)
	w("hermes/cron/jobs.json", `{"jobs":[]}`, 0o644)
	w("hermes/workspace/notes.txt", "hello", 0o644)
	w("gateway/state.json", `{"stopped":false}`, 0o644)
	w("gateway/memory/taste/food.md", "# Food\n- No okra.\n", 0o644)
	w("gateway/memory/.git/HEAD", "ref: refs/heads/main\n", 0o644)
	if err := os.MkdirAll(filepath.Join(home, "hermes", "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	aw, err := audit.Open(filepath.Join(home, "gateway", "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		aw.Append(audit.Record{Tool: "ping", Kind: "read", Decision: "allow", Args: json.RawMessage(`{}`)})
	}
	aw.Close()
	return home
}

func create(t *testing.T, home string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	a, err := Create(Options{Home: home, OutDir: out, Now: t0, Host: "forge", CheckDB: okCheck})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return a
}

// treeHash lists every path under home/{hermes,gateway} with its mode and content hash.
func treeHash(t *testing.T, home string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, top := range []string{"hermes", "gateway"} {
		filepath.WalkDir(filepath.Join(home, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(home, p)
			info, _ := d.Info()
			if d.IsDir() {
				out[rel+"/"] = fmt.Sprintf("dir %o", info.Mode().Perm())
				return nil
			}
			b, _ := os.ReadFile(p)
			s := sha256.Sum256(b)
			out[rel] = fmt.Sprintf("%o %s", info.Mode().Perm(), hex.EncodeToString(s[:]))
			return nil
		})
	}
	return out
}

func sameTree(t *testing.T, a, b map[string]string) {
	t.Helper()
	var keys []string
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a[k] != b[k] {
			t.Errorf("%s differs: %q vs %q", k, a[k], b[k])
		}
	}
}

// rewrite builds a modified copy of an archive. fn sees each entry and returns the body to
// write (or nil to drop it); extra entries are appended. The .sha256 sidecar is recomputed,
// so the result looks like a legitimate upload and only the manifest can catch it.
func rewrite(t *testing.T, archive string, fn func(h *tar.Header, body []byte) []byte, extra func(tw *tar.Writer)) string {
	t.Helper()
	in, _ := os.Open(archive)
	defer in.Close()
	gz, _ := gzip.NewReader(in)
	tr := tar.NewReader(gz)
	dst := filepath.Join(t.TempDir(), "evil.tar.gz")
	f, _ := os.Create(dst)
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if h.Name == manifestName && extra != nil {
			extra(tw) // before the manifest, so the extras reach the checks that matter
			extra = nil
		}
		nb := fn(h, body)
		if nb == nil {
			continue
		}
		h.Size = int64(len(nb))
		tw.WriteHeader(h)
		tw.Write(nb)
	}
	tw.Close()
	gw.Close()
	f.Close()
	b, _ := os.ReadFile(dst)
	s := sha256.Sum256(b)
	os.WriteFile(dst+".sha256", []byte(hex.EncodeToString(s[:])+"  "+filepath.Base(dst)+"\n"), 0o600)
	return dst
}

// --- create and restore ------------------------------------------------------

func TestRoundTripRestoresEveryFileAndMode(t *testing.T) {
	src := seed(t)
	a := create(t, src)
	dst := filepath.Join(t.TempDir(), ".chhaya")
	if err := Restore(a, dst, okCheck); err != nil {
		t.Fatalf("restore: %v", err)
	}
	sameTree(t, treeHash(t, src), treeHash(t, dst))
	for _, d := range []string{"hermes", "gateway"} {
		if fi, _ := os.Stat(filepath.Join(dst, d)); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s must be 0700 after restore, got %o", d, fi.Mode().Perm())
		}
	}
}

func TestCreateWritesAChecksumSidecarAndNamesTheArchiveByHostAndTime(t *testing.T) {
	a := create(t, seed(t))
	if !strings.HasSuffix(a, "chhaya-data-forge-20261110-203000.tar.gz") {
		t.Errorf("unexpected archive name %q", a)
	}
	b, err := os.ReadFile(a + ".sha256")
	if err != nil || !strings.Contains(string(b), "chhaya-data-forge-20261110-203000.tar.gz") {
		t.Errorf("missing or malformed sidecar: %v %q", err, b)
	}
	if fi, _ := os.Stat(a); fi.Mode().Perm() != 0o600 {
		t.Errorf("archive must be 0600, got %o", fi.Mode().Perm())
	}
}

func TestCreateLeavesLocksAndSocketsOut(t *testing.T) {
	src := seed(t)
	os.WriteFile(filepath.Join(src, "hermes", "gateway.lock"), []byte("pid 123"), 0o600)
	os.WriteFile(filepath.Join(src, "hermes", "x.sock"), nil, 0o600)
	dst := filepath.Join(t.TempDir(), ".chhaya")
	if err := Restore(create(t, src), dst, okCheck); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"hermes/gateway.lock", "hermes/x.sock"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s must not travel: a pid or socket from another machine means nothing", p)
		}
	}
}

func TestCreateRefusesWhenADatabaseFailsItsIntegrityCheck(t *testing.T) {
	src := seed(t)
	bad := func(p string) error {
		if strings.HasSuffix(p, "state.db") {
			return errors.New("database disk image is malformed")
		}
		return nil
	}
	_, err := Create(Options{Home: src, OutDir: t.TempDir(), Now: t0, Host: "h", CheckDB: bad})
	if err == nil || !strings.Contains(err.Error(), "state.db") {
		t.Fatalf("a corrupt database must stop the backup and name the file: %v", err)
	}
}

func TestCreateOnlyChecksDatabasesNotOtherFiles(t *testing.T) {
	var seen []string
	rec := func(p string) error { seen = append(seen, filepath.Base(p)); return nil }
	Create(Options{Home: seed(t), OutDir: t.TempDir(), Now: t0, Host: "h", CheckDB: rec})
	if len(seen) != 1 || seen[0] != "state.db" {
		t.Errorf("only *.db files are SQLite checked, saw %v", seen)
	}
}

func TestCreateRefusesATamperedAuditChain(t *testing.T) {
	src := seed(t)
	p := filepath.Join(src, "gateway", "audit.jsonl")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `"allow"`, `"deny"`, 1)), 0o600)
	if _, err := Create(Options{Home: src, OutDir: t.TempDir(), Now: t0, Host: "h", CheckDB: okCheck}); err == nil {
		t.Fatal("a backup of a broken audit chain must be refused, not quietly preserved as good")
	}
}

func TestCreateRefusesWhenADataDirectoryIsMissing(t *testing.T) {
	for _, gone := range []string{"hermes", "gateway"} {
		src := seed(t)
		os.RemoveAll(filepath.Join(src, gone))
		if _, err := Create(Options{Home: src, OutDir: t.TempDir(), Now: t0, Host: "h", CheckDB: okCheck}); err == nil {
			t.Errorf("missing %s must be refused", gone)
		}
	}
}

func TestCreateRefusesSymlinksAndOtherUnexpectedFiles(t *testing.T) {
	src := seed(t)
	os.Symlink("/etc/passwd", filepath.Join(src, "hermes", "link"))
	if _, err := Create(Options{Home: src, OutDir: t.TempDir(), Now: t0, Host: "h", CheckDB: okCheck}); err == nil {
		t.Fatal("a symlink is not data we know how to restore safely; refuse")
	}
}

// --- restore refusals --------------------------------------------------------

func TestRestoreNeverOverwritesExistingData(t *testing.T) {
	a := create(t, seed(t))
	for _, d := range []string{"hermes", "gateway"} {
		dst := filepath.Join(t.TempDir(), ".chhaya")
		os.MkdirAll(filepath.Join(dst, d), 0o700)
		keep := filepath.Join(dst, d, "mine")
		os.WriteFile(keep, []byte("precious"), 0o600)
		if err := Restore(a, dst, okCheck); err == nil {
			t.Errorf("restore over a non-empty %s must be refused", d)
		}
		if b, _ := os.ReadFile(keep); string(b) != "precious" {
			t.Errorf("existing %s data was touched", d)
		}
	}
}

func TestRestoreAcceptsEmptyExistingDirectories(t *testing.T) {
	a := create(t, seed(t))
	dst := filepath.Join(t.TempDir(), ".chhaya")
	os.MkdirAll(filepath.Join(dst, "hermes"), 0o700)
	os.MkdirAll(filepath.Join(dst, "gateway"), 0o700)
	if err := Restore(a, dst, okCheck); err != nil {
		t.Fatalf("empty directories are not data: %v", err)
	}
}

func TestRestoreRefusesACorruptedArchive(t *testing.T) {
	a := create(t, seed(t))
	b, _ := os.ReadFile(a)
	b[len(b)/2] ^= 0xff
	os.WriteFile(a, b, 0o600)
	dst := filepath.Join(t.TempDir(), ".chhaya")
	if err := Restore(a, dst, okCheck); err == nil {
		t.Fatal("a flipped byte must fail the checksum")
	}
	assertEmpty(t, dst)
}

func TestRestoreRefusesAnArchiveWithNoChecksumSidecar(t *testing.T) {
	a := create(t, seed(t))
	os.Remove(a + ".sha256")
	if err := Restore(a, filepath.Join(t.TempDir(), ".chhaya"), okCheck); err == nil {
		t.Fatal("no sidecar means no integrity claim; refuse")
	}
}

func TestRestoreCatchesAFileChangedInsideAnArchiveWithAValidChecksum(t *testing.T) {
	// Someone with write access to the bucket can fix up the sidecar. The manifest, which
	// records every file's hash, is what catches the edit.
	a := create(t, seed(t))
	evil := rewrite(t, a, func(h *tar.Header, body []byte) []byte {
		if h.Name == "hermes/.env" {
			return []byte("SECRET=attacker\n")
		}
		return body
	}, nil)
	dst := filepath.Join(t.TempDir(), ".chhaya")
	if err := Restore(evil, dst, okCheck); err == nil {
		t.Fatal("a file that no longer matches the manifest must be refused")
	}
	assertEmpty(t, dst)
}

func TestRestoreCatchesAFileAddedToTheArchive(t *testing.T) {
	a := create(t, seed(t))
	evil := rewrite(t, a, func(h *tar.Header, b []byte) []byte { return b }, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "hermes/skills/backdoor.py", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
		tw.Write([]byte("bad"))
	})
	dst := filepath.Join(t.TempDir(), ".chhaya")
	if err := Restore(evil, dst, okCheck); err == nil {
		t.Fatal("a file not in the manifest must be refused")
	}
	assertEmpty(t, dst)
}

func TestRestoreCatchesAFileMissingFromTheArchive(t *testing.T) {
	a := create(t, seed(t))
	evil := rewrite(t, a, func(h *tar.Header, b []byte) []byte {
		if h.Name == "gateway/state.json" {
			return nil
		}
		return b
	}, nil)
	if err := Restore(evil, filepath.Join(t.TempDir(), ".chhaya"), okCheck); err == nil {
		t.Fatal("a file the manifest lists but the archive lacks must be refused")
	}
}

func TestRestoreRefusesPathsThatEscapeTheTarget(t *testing.T) {
	for _, name := range []string{"../escape", "hermes/../../escape", "/etc/escape", "other/x"} {
		a := create(t, seed(t))
		evil := rewrite(t, a, func(h *tar.Header, b []byte) []byte { return b }, func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
			tw.Write([]byte("x"))
		})
		parent := t.TempDir()
		dst := filepath.Join(parent, "sub", ".chhaya")
		if err := Restore(evil, dst, okCheck); err == nil {
			t.Errorf("entry %q must be refused", name)
		}
		filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Name() == "escape" {
				t.Errorf("entry %q was written to %s", name, p)
			}
			return nil
		})
	}
}

func TestRestoreRefusesSymlinkAndDeviceEntries(t *testing.T) {
	a := create(t, seed(t))
	evil := rewrite(t, a, func(h *tar.Header, b []byte) []byte { return b }, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "hermes/link", Typeflag: tar.TypeSymlink, Linkname: "/etc"})
	})
	if err := Restore(evil, filepath.Join(t.TempDir(), ".chhaya"), okCheck); err == nil {
		t.Fatal("a symlink entry must be refused")
	}
}

func TestRestoreRunsTheIntegrityCheckOnWhatItRestoredAndLeavesNothingOnFailure(t *testing.T) {
	a := create(t, seed(t))
	dst := filepath.Join(t.TempDir(), ".chhaya")
	bad := func(p string) error { return errors.New("malformed") }
	if err := Restore(a, dst, bad); err == nil {
		t.Fatal("a restored database that fails its check must fail the restore")
	}
	assertEmpty(t, dst)
	if err := Restore(a, dst, okCheck); err != nil {
		t.Fatalf("a failed restore must leave the way clear for a retry: %v", err)
	}
}

func TestRestoreChecksTheAuditChainOfWhatItRestored(t *testing.T) {
	a := create(t, seed(t))
	evil := rewrite(t, a, func(h *tar.Header, b []byte) []byte { return b }, nil)
	// Same bytes, so this passes; the point is that the chain check runs on restore too.
	dst := filepath.Join(t.TempDir(), ".chhaya")
	if err := Restore(evil, dst, okCheck); err != nil {
		t.Fatal(err)
	}
	if n, err := audit.Verify(filepath.Join(dst, "gateway", "audit.jsonl")); err != nil || n != 3 {
		t.Errorf("restored chain: %d %v", n, err)
	}
}

func assertEmpty(t *testing.T, home string) {
	t.Helper()
	for _, d := range []string{"hermes", "gateway"} {
		es, err := os.ReadDir(filepath.Join(home, d))
		if err == nil && len(es) > 0 {
			t.Errorf("%s was left half-restored: %d entries", d, len(es))
		}
	}
	if es, err := os.ReadDir(home); err == nil {
		for _, e := range es {
			if strings.HasPrefix(e.Name(), ".restore-") {
				t.Errorf("temporary directory %s was left behind", e.Name())
			}
		}
	}
}

// --- verify ------------------------------------------------------------------

func TestVerifyPassesAGoodArchiveAndFailsABadOne(t *testing.T) {
	a := create(t, seed(t))
	if err := Verify(a); err != nil {
		t.Fatalf("good archive: %v", err)
	}
	b, _ := os.ReadFile(a)
	b[len(b)/3] ^= 0xff
	os.WriteFile(a, b, 0o600)
	if err := Verify(a); err == nil {
		t.Fatal("corrupted archive must fail verify")
	}
}

// --- push, pull, status --------------------------------------------------------

type calls [][]string

func (c *calls) run(name string, args ...string) error {
	*c = append(*c, append([]string{name}, args...))
	return nil
}

func TestPushUploadsArchiveAndSidecarThenRecordsSuccess(t *testing.T) {
	home := seed(t)
	a := create(t, home)
	var c calls
	if err := Push(home, a, "s3://bkt/backups/", c.run, t0); err != nil {
		t.Fatal(err)
	}
	if len(c) != 2 || c[0][0] != "aws" || c[0][1] != "s3" || c[0][2] != "cp" {
		t.Fatalf("expected two aws s3 cp calls, got %v", c)
	}
	want := "s3://bkt/backups/" + filepath.Base(a)
	if c[0][len(c[0])-1] != want || c[1][len(c[1])-1] != want+".sha256" {
		t.Errorf("wrong destinations: %v", c)
	}
	if err := Status(home, time.Hour, t0.Add(time.Minute)); err != nil {
		t.Errorf("status after a successful push: %v", err)
	}
}

func TestAFailedUploadRecordsNoSuccess(t *testing.T) {
	home := seed(t)
	a := create(t, home)
	fail := func(string, ...string) error { return errors.New("AccessDenied") }
	if err := Push(home, a, "s3://bkt/b", fail, t0); err == nil {
		t.Fatal("push must report the upload failure")
	}
	if err := Status(home, 100*time.Hour, t0); err == nil {
		t.Fatal("no success may be recorded for a failed upload")
	}
}

func TestPushRefusesAnArchiveThatDoesNotVerify(t *testing.T) {
	home := seed(t)
	a := create(t, home)
	b, _ := os.ReadFile(a)
	b[len(b)/2] ^= 0xff
	os.WriteFile(a, b, 0o600)
	var c calls
	if err := Push(home, a, "s3://bkt/b", c.run, t0); err == nil || len(c) != 0 {
		t.Fatalf("a bad archive must not be uploaded: %v %v", err, c)
	}
}

func TestPullFetchesTheArchiveAndItsSidecar(t *testing.T) {
	var c calls
	dir := t.TempDir()
	if err := Pull("s3://bkt/backups/chhaya-data-x.tar.gz", dir, c.run); err != nil {
		t.Fatal(err)
	}
	if len(c) != 2 || c[1][len(c[1])-2] != "s3://bkt/backups/chhaya-data-x.tar.gz.sha256" {
		t.Errorf("unexpected calls: %v", c)
	}
}

func TestStatusFailsWhenThereIsNoSuccessOrItIsTooOld(t *testing.T) {
	home := seed(t)
	if err := Status(home, time.Hour, t0); err == nil {
		t.Error("no backup ever recorded must be an error")
	}
	a := create(t, home)
	var c calls
	Push(home, a, "s3://b/x", c.run, t0)
	if err := Status(home, 36*time.Hour, t0.Add(35*time.Hour)); err != nil {
		t.Errorf("35h old with a 36h limit: %v", err)
	}
	if err := Status(home, 36*time.Hour, t0.Add(37*time.Hour)); err == nil {
		t.Error("37h old with a 36h limit must fail")
	}
}

func TestRestoreCatchesAChangedFileMode(t *testing.T) {
	// Same content, but .env made world-readable: only the mode check can see it.
	a := create(t, seed(t))
	evil := rewrite(t, a, func(h *tar.Header, b []byte) []byte {
		if h.Name == "hermes/.env" {
			h.Mode = 0o644
		}
		return b
	}, nil)
	dst := filepath.Join(t.TempDir(), ".chhaya")
	if err := Restore(evil, dst, okCheck); err == nil {
		t.Fatal("a mode that differs from the manifest must be refused")
	}
	assertEmpty(t, dst)
}
