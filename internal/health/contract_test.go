package health

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// The old pipeline's sidecar sent HRV as last_night_avg while its validator wanted average_hrv, and
// nothing noticed for months because each half was tested against itself. This runs the sidecar's
// real transform on realistic Garmin responses and pushes its output through the gateway's strict
// decoder. It skips, loudly, only when there is no python3.
func TestTheSidecarsDocumentIsAcceptedByTheGateway(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed: the sidecar contract test is skipped")
	}
	cmd := exec.Command(py, "-I", "sync/garmin/tests/contract_doc.py")
	cmd.Dir = "../.."
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the sidecar's transform failed: %v\n%s", err, stderr.String())
	}
	d, err := Decode(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("the gateway refused what the sidecar sends: %v\n%s", err, out.String())
	}
	ms, acts, err := d.Rows(ist, now)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	// every section the fixture has must arrive: a renamed field would silently drop out here
	if len(ms) != 7+2+3+10+4 || len(acts) != 1 {
		t.Fatalf("%d metrics, %d activities arrived: %v\n%s", len(ms), len(acts), names(ms), out.String())
	}
}
