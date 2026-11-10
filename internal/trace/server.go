package trace

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
)

//go:embed web/index.html
var indexHTML []byte

const maxBody = 256 << 10

// NewHandler serves the annotation page and its API. allowedHosts are the Host headers
// accepted (127.0.0.1:PORT, localhost:PORT).
//
// The page shows private conversations and can write to the bucket, and anything on this
// machine can open a socket to localhost, including a web page in another browser tab. Two
// defences, both tested: requests whose Host is not ours are refused (DNS rebinding), and
// every write needs a custom header that a cross-site page cannot send without a CORS
// preflight, which this server never answers.
func NewHandler(s *Store, allowedHosts []string) http.Handler {
	allowed := map[string]bool{}
	for _, h := range allowedHosts {
		allowed[h] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/index", func(w http.ResponseWriter, r *http.Request) {
		refs, err := s.Index()
		if err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
		un, err := s.Unsynced()
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]any{"refs": refs, "unsynced": len(un), "can_sync": s.S3 != ""})
	})
	mux.HandleFunc("GET /api/trace/{day}/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, err := s.Trace(r.PathValue("day"), r.PathValue("id"))
		raw(w, b, err)
	})
	mux.HandleFunc("GET /api/annotation/{day}/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, err := s.Annotation(r.PathValue("day"), r.PathValue("id"))
		if err == nil && b == nil {
			b = []byte("null")
		}
		raw(w, b, err)
	})
	mux.HandleFunc("PUT /api/annotation/{day}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !ValidRef(r.PathValue("day"), r.PathValue("id")) {
			fail(w, http.StatusBadRequest, errors.New("bad trace reference"))
			return
		}
		var a Annotation
		dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		if err := s.SaveAnnotation(r.PathValue("day"), r.PathValue("id"), a); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		un, _ := s.Unsynced()
		writeJSON(w, map[string]any{"ok": true, "unsynced": len(un)})
	})
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		n, err := s.Sync()
		un, _ := s.Unsynced()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "pushed": n, "unsynced": len(un)})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "pushed": n, "unsynced": len(un)})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Chhaya") != "1" {
			http.Error(w, "missing X-Chhaya header", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		mux.ServeHTTP(w, r)
	})
}

// HostsFor returns the Host header values for a listener address.
func HostsFor(addr net.Addr) []string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	return []string{"127.0.0.1:" + port, "localhost:" + port}
}

func raw(w http.ResponseWriter, b []byte, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		fail(w, http.StatusNotFound, errors.New("not found"))
	case err != nil && strings.Contains(err.Error(), "bad trace reference"):
		fail(w, http.StatusBadRequest, err)
	case err != nil:
		fail(w, http.StatusBadGateway, err)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
