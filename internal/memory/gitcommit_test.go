package memory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repoWith makes a memory root that is a git repo whose first commit holds the given files.
func repoWith(t *testing.T, files map[string]string) Store {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	root := t.TempDir()
	git(t, root, "init", "-q")
	for rel, body := range files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "initial")
	return Store{Root: root, Commit: GitCommit}
}

func commits(t *testing.T, root string) []string {
	return strings.Split(git(t, root, "log", "--format=%s"), "\n")
}

func TestApprovedWriteIsCommittedAloneWithItsProposalID(t *testing.T) {
	s := repoWith(t, map[string]string{
		"interests.md":  "# Interests\n\n",
		"taste/food.md": "# Food\n\n",
	})
	// an unrelated edit that is not committed yet must stay uncommitted
	if err := os.WriteFile(filepath.Join(s.Root, "taste", "food.md"), []byte("# Food\n\n- hand edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := stage(t, s, "interests", "Loves books.")
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	log := commits(t, s.Root)
	if len(log) != 2 || !strings.Contains(log[0], p.ID) || !strings.Contains(log[0], "interests") {
		t.Fatalf("want one new commit naming the proposal and document, got %v", log)
	}
	if files := git(t, s.Root, "show", "--name-only", "--format=", "HEAD"); files != "interests.md" {
		t.Fatalf("the commit must hold only the written document, got %q", files)
	}
	if st := git(t, s.Root, "status", "--porcelain", "--untracked-files=no"); !strings.Contains(st, "taste/food.md") {
		t.Fatalf("the unrelated edit must stay uncommitted, status: %q", st)
	}
}

func TestApprovedWriteToANewDocumentIsCommitted(t *testing.T) {
	s := repoWith(t, map[string]string{"interests.md": "# Interests\n\n", "taste/food.md": "# Food\n\n"})
	p := stage(t, s, "taste/music", "Likes ghazals.")
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	if files := git(t, s.Root, "show", "--name-only", "--format=", "HEAD"); files != "taste/music.md" {
		t.Fatalf("got %q", files)
	}
}

// If the commit fails the line is already in the file. The proposal must stay pending so the
// owner's second tap retries, and the retry must not write the line twice.
func TestFailedCommitLeavesTheProposalPendingAndARetryFinishesIt(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git binary")
	}
	root := t.TempDir() // not a git repo yet
	if err := os.WriteFile(filepath.Join(root, "interests.md"), []byte("# Interests\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: root, Commit: GitCommit}
	p := stage(t, s, "interests", "Loves books.")
	if _, err := s.ApplyApproved(p.ID, t1030); err == nil {
		t.Fatal("a write that could not be committed must be an error")
	}
	if ready, _, _, _ := s.Pending(); len(ready) != 1 {
		t.Fatalf("the proposal must still be pending, got %d", len(ready))
	}
	git(t, root, "init", "-q") // the cause is fixed
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if n := strings.Count(readFile(t, filepath.Join(root, "interests.md")), p.ID); n != 1 {
		t.Fatalf("the line must be written once, found it %d times", n)
	}
	if log := git(t, root, "log", "--format=%s"); !strings.Contains(log, p.ID) {
		t.Fatalf("the retry must have committed: %q", log)
	}
	if ready, _, _, _ := s.Pending(); len(ready) != 0 {
		t.Fatal("the proposal must be applied after the retry")
	}
}

func TestCommittingTwiceDoesNotMakeAnEmptyCommit(t *testing.T) {
	s := repoWith(t, map[string]string{"interests.md": "# Interests\n\n"})
	p := stage(t, s, "interests", "Loves books.")
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(s.Root, "interests.md")
	if err := GitCommit(s.Root, doc, "again"); err != nil {
		t.Fatalf("nothing to commit is not an error: %v", err)
	}
	if n := len(commits(t, s.Root)); n != 2 {
		t.Fatalf("want 2 commits, got %d", n)
	}
}

// The gateway runs in a container the memory repo's own hooks and the host's git config must not
// reach into.
func TestCommitIgnoresRepoHooksAndGlobalConfig(t *testing.T) {
	s := repoWith(t, map[string]string{"interests.md": "# Interests\n\n"})
	hook := filepath.Join(s.Root, ".git", "hooks", "pre-commit")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := stage(t, s, "interests", "Loves books.")
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatalf("a pre-commit hook in the memory repo must not block or run: %v", err)
	}
}

func TestStoreWithNoCommitHookStillWrites(t *testing.T) { // the unit-test and dev default
	s := Store{Root: t.TempDir()}
	p := stage(t, s, "interests", "Loves books.")
	if _, err := s.ApplyApproved(p.ID, t1030); err != nil {
		t.Fatal(err)
	}
}
