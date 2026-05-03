// Package volume parses "-v" flags, resolves named volume paths, and performs
// bind-mount operations that make host directories (or managed volumes) visible
// inside a container's overlay merged directory.
package volume

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Kind distinguishes how the volume source should be resolved.
type Kind int

const (
	Bind  Kind = iota // source is an absolute host path (bind mount)
	Named             // source is a named or anonymous managed volume
)

// Spec is a parsed "-v host:container[:ro]" flag — one mount request.
type Spec struct {
	Kind     Kind   // Bind or Named
	Source   string // host path (Bind) or volume name (Named)
	Target   string // absolute path inside the container
	ReadOnly bool   // true when ":ro" was specified
}

// Parse decodes a single "-v" flag value into a Spec. Accepted forms:
//   - "/container/path"       — anonymous named volume mounted at that path
//   - "name:/container/path"  — named volume
//   - "/host:/container/path[:ro|:rw]" — bind mount
func Parse(s string) (*Spec, error) {
	if !strings.Contains(s, ":") {
		if !strings.HasPrefix(s, "/") {
			return nil, fmt.Errorf("volume spec %q: expected src:dst[:mode] or /container/path", s)
		}
		return &Spec{Kind: Named, Source: generateAnonymousName(), Target: s, ReadOnly: false}, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return nil, fmt.Errorf("invalid volume spec %q: expected src:dst[:mode]", s)
	}
	source, target := parts[0], parts[1]
	if source == "" {
		return nil, errors.New("volume spec: source is empty")
	}
	if target == "" {
		return nil, errors.New("volume spec: target is empty")
	}
	if !strings.HasPrefix(target, "/") {
		return nil, fmt.Errorf("volume spec: target %q must be absolute", target)
	}

	var readOnly bool

	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			readOnly = true
		case "rw":
			readOnly = false
		default:
			return nil, fmt.Errorf("volume spec: mode %q must be 'ro' or 'rw'", parts[2])
		}
	}

	var kind Kind

	if strings.HasPrefix(source, "/") {
		kind = Bind
	} else {
		if strings.Contains(source, "/") {
			return nil, errors.New("named volume must not contain slashes")
		}
		kind = Named
	}

	return &Spec{Kind: kind, Source: source, Target: target, ReadOnly: readOnly}, nil
}

// generateAnonymousName creates a random volume name for bare container-path
// specs (e.g. "-v /data"). This matches Docker's behaviour of auto-creating a
// named volume when no explicit source is given.
func generateAnonymousName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "anon_" + hex.EncodeToString(b)
}
