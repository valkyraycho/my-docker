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

var ErrNotFound = errors.New("container not found")

type Registry struct {
	mu         sync.RWMutex
	containers map[string]*Container
}

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

func (r *Registry) List() ([]*Container, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	all := make([]*Container, 0, len(r.containers))
	for _, c := range r.containers {
		all = append(all, c)
	}
	return all, nil
}

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

func (r *Registry) Get(id string) (*Container, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.containers[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

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

func List() ([]*Container, error) {
	r, err := NewRegistry()
	if err != nil {
		return nil, err
	}
	return r.List()
}

func Find(prefix string) (*Container, error) {
	r, err := NewRegistry()
	if err != nil {
		return nil, err
	}
	return r.Find(prefix)
}
