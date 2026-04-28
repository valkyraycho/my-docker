package volume

import (
	"errors"
	"fmt"
	"strings"
)

type Kind int

const (
	Bind Kind = iota
	Named
)

type Spec struct {
	Kind     Kind
	Source   string
	Target   string
	ReadOnly bool
}

func Parse(s string) (*Spec, error) {
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
