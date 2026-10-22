// Package config loads config/policy.yaml. The file supplies values; it never
// supplies rules. Anything absent or unreadable is an error, not a default,
// because the permissive reading of a typo is the one we must never take.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/satishthakur/chhaya/internal/policy"
	"gopkg.in/yaml.v3"
)

type file struct {
	Caps struct {
		Food struct {
			PerOrderINR *int64 `yaml:"per_order_inr"`
			PerDayINR   *int64 `yaml:"per_day_inr"`
		} `yaml:"food"`
		RateLimits struct {
			DefaultPerHour *int `yaml:"default_per_hour"`
		} `yaml:"rate_limits"`
	} `yaml:"caps"`
	Allowlists struct {
		Repos        []string `yaml:"repos"`
		BranchPrefix *string  `yaml:"branch_prefix"`
	} `yaml:"allowlists"`
	Approvals struct {
		TimeoutSeconds *int `yaml:"timeout_seconds"`
	} `yaml:"approvals"`
}

// Load reads and validates the policy file. It returns the config, the hex
// sha256 of the raw file bytes, and an error. The hash is what the startup
// card keys on, so it covers comments and formatting too: if the file changed
// at all, we would rather tell you.
func Load(path string) (policy.Config, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return policy.Config{}, "", fmt.Errorf("read policy file: %w", err)
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	var f file
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // an unknown key is a typo or a stale assumption; refuse both
	if err := dec.Decode(&f); err != nil {
		return policy.Config{}, hash, fmt.Errorf("parse %s: %w", path, err)
	}

	// Pointers everywhere above so that "absent" and "zero" are different
	// things. Absent is an error; zero is an error too, but a different one.
	perOrder, err := required(f.Caps.Food.PerOrderINR, "caps.food.per_order_inr")
	if err != nil {
		return policy.Config{}, hash, err
	}
	perDay, err := required(f.Caps.Food.PerDayINR, "caps.food.per_day_inr")
	if err != nil {
		return policy.Config{}, hash, err
	}
	rate, err := required(f.Caps.RateLimits.DefaultPerHour, "caps.rate_limits.default_per_hour")
	if err != nil {
		return policy.Config{}, hash, err
	}
	timeout, err := required(f.Approvals.TimeoutSeconds, "approvals.timeout_seconds")
	if err != nil {
		return policy.Config{}, hash, err
	}
	if f.Allowlists.BranchPrefix == nil {
		return policy.Config{}, hash, fmt.Errorf("allowlists.branch_prefix is required")
	}
	if perDay < perOrder {
		return policy.Config{}, hash, fmt.Errorf(
			"caps.food.per_day_inr (%d) is below per_order_inr (%d), which cannot be what you meant", perDay, perOrder)
	}

	return policy.Config{
		Caps: map[string]policy.Cap{
			"food": {PerOrderMinor: perOrder * 100, PerDayMinor: perDay * 100},
		},
		DefaultRatePerHour: rate,
		ApprovalTimeout:    time.Duration(timeout) * time.Second,
		BranchPrefix:       *f.Allowlists.BranchPrefix,
		// Empty means no repository is writable, which is the safe default.
		// Do not normalise to nil: yaml.v3 decodes an explicit `repos: []` to a
		// non-nil zero-length slice and only an omitted key to nil, and every
		// consumer tests len(), so the representation is not the invariant.
		Repos: f.Allowlists.Repos,
	}, hash, nil
}

type number interface{ ~int | ~int64 }

func required[T number](v *T, key string) (T, error) {
	var zero T
	if v == nil {
		return zero, fmt.Errorf("%s is required and has no safe default", key)
	}
	if *v <= 0 {
		return zero, fmt.Errorf("%s must be positive; to forbid something, remove the tool rather than setting %v", key, *v)
	}
	return *v, nil
}
