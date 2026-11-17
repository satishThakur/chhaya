package memory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitCommit commits one document of the memory repo with the given message. It is the Commit hook
// the gateway sets, so every approved write is its own commit and "revert the last write" is a
// git revert, not a guess (docs/dreaming.md).
//
// It commits only doc, so edits that are still uncommitted elsewhere are not swept in. It is safe to
// call twice: when the document has nothing new to commit it does nothing. It runs git with no
// system or global config, no hooks and no signing, because the gateway must behave the same on any
// host and a hook in the memory repo is not code the gateway should run.
func GitCommit(root, doc, message string) error {
	absRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, doc)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("document %q is not inside the memory root", doc)
	}
	run := func(args ...string) (int, string, error) {
		base := []string{"-C", absRoot,
			"-c", "user.name=chhaya-gateway", "-c", "user.email=gateway@chhaya.local",
			"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
			"-c", "safe.directory=" + absRoot}
		cmd := exec.Command("git", append(base, args...)...)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.TempDir(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), out.String(), nil
		}
		return 0, out.String(), err
	}
	fail := func(what string, code int, out string) error {
		return fmt.Errorf("git %s failed (exit %d): %s", what, code, strings.TrimSpace(out))
	}

	if code, out, err := run("add", "--", rel); err != nil {
		return err
	} else if code != 0 {
		return fail("add", code, out)
	}
	code, out, err := run("diff", "--cached", "--quiet", "--", rel)
	if err != nil {
		return err
	}
	switch code {
	case 0:
		return nil // nothing new: already committed
	case 1:
	default:
		return fail("diff", code, out)
	}
	if code, out, err := run("commit", "-q", "-m", message, "--", rel); err != nil {
		return err
	} else if code != 0 {
		return fail("commit", code, out)
	}
	return nil
}
