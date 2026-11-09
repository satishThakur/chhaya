package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/satishthakur/chhaya/internal/backup"
)

// backupDeps are the things a test replaces: the clock, the aws CLI and the SQLite check.
type backupDeps struct {
	now   func() time.Time
	run   backup.Runner
	check backup.Checker
}

func realBackupDeps() backupDeps {
	return backupDeps{
		now:   time.Now,
		check: backup.SQLiteChecker(),
		run: func(name string, args ...string) error {
			out, err := exec.Command(name, args...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s: %v: %s", name, err, strings.TrimSpace(string(out)))
			}
			return nil
		},
	}
}

func defaultHome() string {
	if h := os.Getenv("CHHAYA_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".chhaya")
}

const backupUsage = `chhayactl backup <command>   (run from the host, with the stack stopped)

  create  [-home DIR] [-out DIR]           write a verified tarball; prints its path
  verify  FILE                             check the checksum and every file against the manifest
  restore [-home DIR] FILE                 extract into DIR/hermes and DIR/gateway; never overwrites
  push    [-home DIR] -s3 URI FILE         upload FILE and its checksum, then record the success
  pull    -dir DIR S3URI                   download an archive and its checksum
  status  [-home DIR] [-max-age 36h]       fail if the last successful upload is too old
`

// runBackup writes only the archive path (create) or a one-line result to out, so shell
// callers can capture it; everything else is an error.
func runBackup(args []string, out io.Writer, d backupDeps) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, backupUsage)
		return errors.New("backup needs a command")
	}
	fs := flag.NewFlagSet("backup "+args[0], flag.ContinueOnError)
	home := fs.String("home", defaultHome(), "the .chhaya directory")
	outDir := fs.String("out", "", "where to write the archive (default HOME/backups)")
	s3 := fs.String("s3", "", "s3://bucket/prefix to upload to")
	dir := fs.String("dir", "", "directory to download into")
	maxAge := fs.Duration("max-age", 36*time.Hour, "oldest acceptable successful backup")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	rest := fs.Args()
	one := func() (string, error) {
		if len(rest) != 1 {
			return "", fmt.Errorf("backup %s takes exactly one argument", args[0])
		}
		return rest[0], nil
	}

	switch args[0] {
	case "create":
		o := *outDir
		if o == "" {
			o = filepath.Join(*home, "backups")
		}
		host, _ := os.Hostname()
		host, _, _ = strings.Cut(host, ".")
		a, err := backup.Create(backup.Options{Home: *home, OutDir: o, Now: d.now(), Host: host, CheckDB: d.check})
		if err != nil {
			return err
		}
		fmt.Fprintln(out, a)
		return nil
	case "verify":
		f, err := one()
		if err != nil {
			return err
		}
		if err := backup.Verify(f); err != nil {
			return err
		}
		fmt.Fprintf(out, "ok: %s verifies\n", filepath.Base(f))
		return nil
	case "restore":
		f, err := one()
		if err != nil {
			return err
		}
		if err := backup.Restore(f, *home, d.check); err != nil {
			return err
		}
		fmt.Fprintf(out, "ok: restored into %s\n", *home)
		return nil
	case "push":
		f, err := one()
		if err != nil {
			return err
		}
		if *s3 == "" {
			return errors.New("push needs -s3 s3://bucket/prefix")
		}
		if err := backup.Push(*home, f, *s3, d.run, d.now()); err != nil {
			return err
		}
		fmt.Fprintf(out, "ok: uploaded %s\n", filepath.Base(f))
		return nil
	case "pull":
		u, err := one()
		if err != nil {
			return err
		}
		if *dir == "" {
			return errors.New("pull needs -dir DIR")
		}
		return backup.Pull(u, *dir, d.run)
	case "status":
		if err := backup.Status(*home, *maxAge, d.now()); err != nil {
			return err
		}
		fmt.Fprintln(out, "ok: a recent backup was uploaded")
		return nil
	}
	fmt.Fprint(os.Stderr, backupUsage)
	return fmt.Errorf("unknown backup command %q", args[0])
}
