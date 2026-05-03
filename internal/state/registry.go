//go:build linux

package state

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

// ErrNotFound is returned when a container ID or prefix has no match.
var ErrNotFound = errors.New("container not found")

// Registry is an in-memory cache of all known containers, backed by the
// directory structure under /var/lib/mydocker/containers. It is loaded once
// at daemon start-up by NewRegistry and kept in sync via Add, Update, and
// Remove. All methods are safe for concurrent use.
type Registry struct {
	mu         sync.RWMutex
	containers map[string]*Container
}

// NewRegistry scans containersDir, loads every valid state.json it finds, and
// returns a populated Registry. Missing or unreadable state files are logged
// and skipped rather than treated as fatal errors.
func NewRegistry() (*Registry, error) {
	containers := make(map[string]*Container)
	entries, err := os.ReadDir(containersDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Registry{containers: containers}, nil
		}
		return nil, fmt.Errorf("read container directory %s: %w", containersDir, err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, err := Load(e.Name())
		if err != nil {
			log.Printf("state: skipping unreadable container %s: %v", e.Name(), err)
			continue
		}
		containers[c.ID] = c
	}
	return &Registry{containers: containers}, nil
}

// List returns all containers currently held in the registry.
func (r *Registry) List() ([]*Container, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := make([]*Container, 0, len(r.containers))
	for _, c := range r.containers {
		all = append(all, c)
	}
	return all, nil
}

// Find returns the unique container whose ID starts with prefix, mirroring the
// Docker short-ID UX. Returns ErrNotFound if no container matches and an error
// listing the ambiguous IDs if more than one matches.
func (r *Registry) Find(prefix string) (*Container, error) {
	if prefix == "" {
		return nil, errors.New("empty prefix")
	}

	all, err := r.List()
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
		return nil, fmt.Errorf("%w: %s", ErrNotFound, prefix)
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

// Get returns the container with the exact given ID, or ErrNotFound.
func (r *Registry) Get(id string) (*Container, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.containers[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

// Add persists c to disk via c.Save and inserts it into the in-memory map.
// Returns an error if a container with the same ID already exists.
func (r *Registry) Add(c *Container) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.containers[c.ID]; ok {
		return fmt.Errorf("container %s already exists", c.ID)
	}
	if err := c.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	r.containers[c.ID] = c
	return nil
}

// Update persists c to disk via c.Save and replaces the in-memory entry.
// Returns ErrNotFound if the container does not exist in the registry.
func (r *Registry) Update(c *Container) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.containers[c.ID]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, c.ID)
	}
	if err := c.Save(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	r.containers[c.ID] = c
	return nil
}

// Remove deletes the container from the in-memory map and removes its state
// directory from disk. Returns ErrNotFound if the ID is unknown.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.containers[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(r.containers, id)
	if err := RemoveDir(id); err != nil {
		return fmt.Errorf("remove state: %w", err)
	}
	return nil
}

// List is a convenience function that creates a one-shot Registry from disk
// and returns all containers. Intended for CLI commands that do not hold a
// long-lived Registry.
func List() ([]*Container, error) {
	r, err := NewRegistry()
	if err != nil {
		return nil, err
	}
	return r.List()
}

// Find is a convenience function that creates a one-shot Registry from disk
// and delegates to Registry.Find. Intended for CLI commands that do not hold a
// long-lived Registry.
func Find(prefix string) (*Container, error) {
	r, err := NewRegistry()
	if err != nil {
		return nil, err
	}
	return r.Find(prefix)
}
