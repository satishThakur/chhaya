package health

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Runner runs an external command (the aws CLI) and returns its stdout.
type Runner func(name string, args ...string) ([]byte, error)

// Puller moves what the desktop's sync left in the bucket into the gateway. The sync (which has
// the Garmin login and a home IP address, which Garmin accepts and AWS's it does not) writes day
// and run documents to <S3>/inbox/; Pull posts each to the gateway's ingest listener, where the
// same strict decoder that guards a direct post validates it, and files it under processed/ or
// rejected/. This side holds no Garmin credential and decides nothing about the data.
type Puller struct {
	S3     string // s3://bucket/health
	URL    string // the ingest listener, e.g. http://127.0.0.1:8444
	Secret string
	Run    Runner
	HTTP   *http.Client
	Max    int // objects per pass; 0 means 200
}

// PullResult is what one pass did.
type PullResult struct {
	Ingested int
	Rejected []string // "name: reason", moved to rejected/
	Left     int      // objects not touched, because the gateway was unreachable or the pass hit Max
}

var inboxName = regexp.MustCompile(`^(day|run)-[0-9A-Za-z-]{1,40}\.json$`)

func (p *Puller) base() string { return strings.TrimRight(p.S3, "/") }

// Pull handles up to Max objects, days before runs. A refusal from the gateway (400, 413) means
// the document is bad: it is moved to rejected/ and the pass goes on. Anything else (the gateway
// down, a 5xx, a 401 from a wrong secret) stops the pass with the object left in place, because
// retrying later is right and moving it would lose it.
func (p *Puller) Pull(ctx context.Context) (PullResult, error) {
	var r PullResult
	bucket, prefix, err := splitS3(p.base() + "/inbox/")
	if err != nil {
		return r, err
	}
	// list-objects-v2, not `s3 ls`: ls exits 1 on an empty prefix, which would make "nothing to do"
	// look like a failure. An empty listing prints "None".
	out, err := p.Run("aws", "s3api", "list-objects-v2", "--bucket", bucket, "--prefix", prefix,
		"--query", "Contents[].Key", "--output", "text")
	if err != nil {
		return r, fmt.Errorf("listing the inbox: %w", err)
	}
	var names []string
	for _, key := range strings.Fields(string(out)) {
		if key != "None" && strings.HasPrefix(key, prefix) {
			names = append(names, strings.TrimPrefix(key, prefix))
		}
	}
	sort.Strings(names) // "day-" sorts before "run-", and dates sort oldest first
	max := p.Max
	if max <= 0 {
		max = 200
	}
	hc := p.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	for i, name := range names {
		if i >= max {
			r.Left += len(names) - i
			break
		}
		if !inboxName.MatchString(name) {
			r.Rejected = append(r.Rejected, name+": not a name the sync writes")
			if err := p.file(name, "rejected"); err != nil {
				return r, err
			}
			continue
		}
		key := p.base() + "/inbox/" + name
		body, err := p.Run("aws", "s3", "cp", "--only-show-errors", key, "-")
		if err != nil {
			return r, fmt.Errorf("reading %s: %w", name, err)
		}
		path := "/ingest/day"
		if strings.HasPrefix(name, "run-") {
			path = "/ingest/run"
		}
		code, reason, err := p.post(ctx, hc, path, body)
		switch {
		case err != nil, code == http.StatusUnauthorized, code >= 500:
			r.Left += len(names) - i
			if err == nil {
				err = fmt.Errorf("the gateway answered %d: %s", code, reason)
			}
			return r, fmt.Errorf("posting %s: %w", name, err)
		case code >= 400:
			r.Rejected = append(r.Rejected, fmt.Sprintf("%s: %s", name, reason))
			if err := p.file(name, "rejected"); err != nil {
				return r, err
			}
		default:
			r.Ingested++
			if err := p.file(name, "processed"); err != nil {
				return r, err
			}
		}
	}
	return r, nil
}

func (p *Puller) post(ctx context.Context, hc *http.Client, path string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Ingest-Secret", p.Secret)
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return resp.StatusCode, strings.TrimSpace(string(b)), nil
}

// file moves an inbox object to processed/ or rejected/. A copy that fails leaves the object in
// the inbox, where the next pass handles it again; a repeat is safe because the upsert is.
func (p *Puller) file(name, to string) error {
	_, err := p.Run("aws", "s3", "mv", "--only-show-errors", p.base()+"/inbox/"+name, p.base()+"/"+to+"/"+name)
	if err != nil {
		return fmt.Errorf("filing %s under %s: %w", name, to, err)
	}
	return nil
}

func splitS3(uri string) (bucket, prefix string, err error) {
	rest, ok := strings.CutPrefix(uri, "s3://")
	b, pre, found := strings.Cut(rest, "/")
	if !ok || !found || b == "" || pre == "" {
		return "", "", fmt.Errorf("%q is not s3://bucket/prefix", uri)
	}
	return b, pre, nil
}
