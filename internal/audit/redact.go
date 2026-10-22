package audit

import (
	"encoding/json"
	"fmt"
	"strings"
)

// secretish names whose values are never logged, whatever the policy said.
// CLAUDE.md requires the logger to redact by default; this is that default.
// It is deliberately a suffix/substring match rather than an exact list,
// because the failure mode we care about is a key nobody thought to name.
var secretish = []string{"token", "secret", "password", "passwd", "api_key", "apikey", "authorization", "credential", "private_key"}

// Redact returns a copy of args with the named paths and any secret-ish key
// replaced by a size marker. Call it before handing args to Append: the whole
// point is that a secret never enters the hashed bytes, so there is no version
// of the log from which it could later be recovered.
//
// paths are dot-separated, e.g. "diff" or "body.content".
func Redact(args map[string]any, paths []string) map[string]any {
	out := deepCopyMap(args)
	for _, p := range paths {
		redactPath(out, strings.Split(p, "."))
	}
	redactSecretish(out)
	return out
}

func redactPath(m map[string]any, parts []string) {
	if len(parts) == 0 {
		return
	}
	k := parts[0]
	v, ok := m[k]
	if !ok {
		return
	}
	if len(parts) == 1 {
		m[k] = marker(v)
		return
	}
	if child, ok := v.(map[string]any); ok {
		redactPath(child, parts[1:])
	}
}

func redactSecretish(m map[string]any) {
	for k, v := range m {
		if child, ok := v.(map[string]any); ok {
			redactSecretish(child)
			continue
		}
		lk := strings.ToLower(k)
		for _, s := range secretish {
			if strings.Contains(lk, s) {
				m[k] = marker(v)
				break
			}
		}
	}
}

// marker records the size of what was removed, so the log still shows that
// something substantial was there without showing what.
func marker(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "<redacted>"
	}
	return fmt.Sprintf("<redacted:%dB>", len(b))
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if child, ok := v.(map[string]any); ok {
			out[k] = deepCopyMap(child)
			continue
		}
		out[k] = v
	}
	return out
}
