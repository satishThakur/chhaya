package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	dayRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	idRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	// keyRe pulls (day, kind, id) out of an object key or a local relative path.
	keyRe = regexp.MustCompile(`days/(\d{4}-\d{2}-\d{2})/(trace|open-code)-([A-Za-z0-9_.-]+)\.json$`)
)

const maxAnnotationField = 20000

// ValidRef reports whether day and id are safe to build a path or key from. Everything
// that reaches the file system or the aws CLI goes through this.
func ValidRef(day, id string) bool {
	return dayRe.MatchString(day) && idRe.MatchString(id) && !strings.Contains(id, "..")
}

// Ref names one trace.
type Ref struct {
	Day       string `json:"day"`
	ID        string `json:"id"`
	Annotated bool   `json:"annotated"`
}

// Annotation is my verdict on one trace (open coding). MessageCount is how many messages
// the trace had when I wrote it, so the page can say the conversation has moved on.
type Annotation struct {
	TraceID      string    `json:"trace_id"`
	Day          string    `json:"day"`
	Pass         *bool     `json:"pass"`
	Note         string    `json:"note"`
	Wanted       string    `json:"wanted"`
	Mode         string    `json:"mode"`
	MessageCount int       `json:"message_count"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Store reads traces and reads and writes annotations. Dir holds the local copy:
// Dir/days/DAY/{trace,open-code}-ID.json, the same layout as the bucket. With S3 set
// (s3://bucket/evals) traces come from the bucket and annotations are pushed there.
type Store struct {
	Dir string
	S3  string
	Run Runner
	Now func() time.Time

	mu    sync.Mutex
	cache map[string][]byte // traces fetched from S3, for the life of the process
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) s3key(day, kind, id string) string {
	return strings.TrimRight(s.S3, "/") + "/days/" + day + "/" + kind + "-" + id + ".json"
}

func (s *Store) local(day, kind, id string) string {
	return filepath.Join(s.Dir, "days", day, kind+"-"+id+".json")
}

// Index lists every trace, oldest first. Annotated is true if an annotation exists
// locally or in the bucket.
func (s *Store) Index() ([]Ref, error) {
	type k struct{ day, id string }
	traces, notes := map[k]bool{}, map[k]bool{}
	add := func(key string) {
		m := keyRe.FindStringSubmatch(filepath.ToSlash(key))
		if m == nil {
			return
		}
		r := k{m[1], m[3]}
		if m[2] == "trace" {
			traces[r] = true
		} else {
			notes[r] = true
		}
	}
	if s.S3 != "" {
		out, err := s.Run("aws", "s3", "ls", "--recursive", strings.TrimRight(s.S3, "/")+"/days/")
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", s.S3, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) >= 4 {
				add(f[len(f)-1])
			}
		}
	}
	err := filepath.WalkDir(filepath.Join(s.Dir, "days"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			add(p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	refs := make([]Ref, 0, len(traces))
	for r := range traces {
		refs = append(refs, Ref{Day: r.day, ID: r.id, Annotated: notes[r]})
	}
	sort.Slice(refs, func(a, b int) bool {
		if refs[a].Day != refs[b].Day {
			return refs[a].Day < refs[b].Day
		}
		return refs[a].ID < refs[b].ID
	})
	return refs, nil
}

// Trace returns one trace's JSON: from the local export directory if it is there,
// otherwise from the bucket (and remembered, so moving back is instant).
func (s *Store) Trace(day, id string) ([]byte, error) {
	if !ValidRef(day, id) {
		return nil, errors.New("bad trace reference")
	}
	if b, err := os.ReadFile(s.local(day, "trace", id)); err == nil {
		return b, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if s.S3 == "" {
		return nil, fs.ErrNotExist
	}
	key := s.s3key(day, "trace", id)
	s.mu.Lock()
	b, ok := s.cache[key]
	s.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := s.Run("aws", "s3", "cp", "--only-show-errors", key, "-")
	if err != nil {
		return nil, fmt.Errorf("fetching trace: %w", err)
	}
	s.mu.Lock()
	if s.cache == nil {
		s.cache = map[string][]byte{}
	}
	s.cache[key] = b
	s.mu.Unlock()
	return b, nil
}

// Annotation returns the saved annotation JSON, local copy first, then the bucket.
// (nil, nil) means there is none yet.
func (s *Store) Annotation(day, id string) ([]byte, error) {
	if !ValidRef(day, id) {
		return nil, errors.New("bad trace reference")
	}
	b, err := os.ReadFile(s.local(day, "open-code", id))
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if s.S3 == "" {
		return nil, nil
	}
	// Absent in the bucket is normal; the aws CLI says so with a non-zero exit, which we
	// cannot tell from a real failure without parsing text, so check the listing first.
	out, lerr := s.Run("aws", "s3", "ls", s.s3key(day, "open-code", id))
	if lerr != nil || len(strings.TrimSpace(string(out))) == 0 {
		return nil, nil
	}
	b, err = s.Run("aws", "s3", "cp", "--only-show-errors", s.s3key(day, "open-code", id), "-")
	if err != nil {
		return nil, fmt.Errorf("fetching annotation: %w", err)
	}
	return b, nil
}

// SaveAnnotation validates and writes an annotation locally. It does not touch the
// bucket; Sync does, when I press Export.
func (s *Store) SaveAnnotation(day, id string, a Annotation) error {
	if !ValidRef(day, id) {
		return errors.New("bad trace reference")
	}
	for name, v := range map[string]string{"note": a.Note, "wanted": a.Wanted, "mode": a.Mode} {
		if len(v) > maxAnnotationField {
			return fmt.Errorf("%s is too long", name)
		}
	}
	a.TraceID, a.Day, a.UpdatedAt = id, day, s.now().UTC()
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.local(day, "open-code", id), b)
}

func (s *Store) syncedPath() string { return filepath.Join(s.Dir, "synced.json") }

func (s *Store) loadSynced() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(s.syncedPath())
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", s.syncedPath(), err)
	}
	return m, nil
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// Unsynced lists local annotations that differ from what was last pushed.
func (s *Store) Unsynced() ([]Ref, error) {
	synced, err := s.loadSynced()
	if err != nil {
		return nil, err
	}
	var out []Ref
	root := filepath.Join(s.Dir, "days")
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		m := keyRe.FindStringSubmatch(filepath.ToSlash(p))
		if d.IsDir() || m == nil || m[2] != "open-code" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if synced[m[1]+"/"+m[3]] != sum(b) {
			out = append(out, Ref{Day: m[1], ID: m[3], Annotated: true})
		}
		return nil
	})
	return out, err
}

// Sync pushes every unsynced annotation and records what it pushed. A failure on one
// leaves it unsynced (the count stays honest) and the rest are still tried.
func (s *Store) Sync() (pushed int, err error) {
	if s.S3 == "" {
		return 0, errors.New("no bucket configured (start with -s3 s3://bucket/evals)")
	}
	todo, err := s.Unsynced()
	if err != nil {
		return 0, err
	}
	synced, err := s.loadSynced()
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, r := range todo {
		b, rerr := os.ReadFile(s.local(r.Day, "open-code", r.ID))
		if rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		if _, perr := s.Run("aws", "s3", "cp", "--only-show-errors", s.local(r.Day, "open-code", r.ID), s.s3key(r.Day, "open-code", r.ID)); perr != nil {
			errs = append(errs, fmt.Errorf("push %s: %w", r.ID, perr))
			continue
		}
		synced[r.Day+"/"+r.ID] = sum(b)
		pushed++
	}
	if pushed > 0 {
		b, _ := json.MarshalIndent(synced, "", "  ")
		if werr := writeAtomic(s.syncedPath(), b); werr != nil {
			errs = append(errs, werr)
		}
	}
	return pushed, errors.Join(errs...)
}
