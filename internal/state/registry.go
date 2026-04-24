//go:build linux

package state

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func List() ([]*Container, error) {
	dirEntries, err := os.ReadDir(containersDir)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return []*Container{}, nil
		default:
			return nil, fmt.Errorf("read container directory %s: %w", containersDir, err)
		}
	}

	result := make([]*Container, 0, len(dirEntries))

	for _, e := range dirEntries {
		if !e.IsDir() {
			continue
		}

		c, err := Load(e.Name())
		if err != nil {
			switch {
			case errors.Is(err, os.ErrNotExist):
				continue
			default:
				return nil, fmt.Errorf("load container state %s: %w", e.Name(), err)
			}
		}

		result = append(result, c)
	}
	return result, nil
}

func Find(prefix string) (*Container, error) {
	if prefix == "" {
		return nil, errors.New("empty prefix")
	}

	all, err := List()
	if err != nil {
		return nil, fmt.Errorf("list container directory: %w", err)
	}

	var matches []*Container
	for _, c := range all {
		if strings.HasPrefix(c.ID, prefix) {
			matches = append(matches, c)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no such container: %s", prefix)
	case 1:
		return matches[0], nil
	default:
		matchIDs := make([]string, 0, len(matches))
		for _, match := range matches {
			matchIDs = append(matchIDs, match.ID)
		}
		return nil, fmt.Errorf("ambiguous prefix %q matches %d containers: %s", prefix, len(matches), strings.Join(matchIDs, ", "))
	}
}
