package services

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
)

// Registry holds the active set of loaded services and supports atomic swap on reload.
type Registry struct {
	mu       sync.RWMutex
	services map[string]*Service
	excluded []ExcludedService
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		services: make(map[string]*Service),
	}
}

// Load atomically replaces the registry contents with the given discovery result.
func (r *Registry) Load(result *DiscoverResult) {
	newMap := make(map[string]*Service, len(result.Services))
	for _, svc := range result.Services {
		newMap[svc.ID] = svc
	}

	r.mu.Lock()
	r.services = newMap
	r.excluded = result.Excluded
	r.mu.Unlock()
}

// Get returns a service by ID, or nil if not found.
func (r *Registry) Get(id string) *Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.services[id]
}

// GetAction returns a service and action by their IDs.
func (r *Registry) GetAction(serviceID, actionID string) (*Service, *Action) {
	svc := r.Get(serviceID)
	if svc == nil {
		return nil, nil
	}
	for i := range svc.Actions {
		if svc.Actions[i].ID == actionID {
			return svc, &svc.Actions[i]
		}
	}
	return svc, nil
}

// All returns a snapshot of all loaded services.
func (r *Registry) All() []*Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*Service, 0, len(r.services))
	for _, svc := range r.services {
		result = append(result, svc)
	}
	return result
}

// Excluded returns the list of excluded services.
func (r *Registry) Excluded() []ExcludedService {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.excluded
}

// RestartHash computes a SHA-256 hash of all runtime-relevant fields for a server-mode service.
// Used to determine whether a server process needs to be restarted on reload.
func RestartHash(svc *Service) string {
	h := sha256.New()
	data, _ := json.Marshal(struct {
		Start          []string
		Env            map[string]string
		Headers        map[string]string
		WorkingDir     string
		HealthCheck    string
		StartupTimeout string
		Platform       string
		Actions        []Action
	}{
		Start:          svc.Start,
		Env:            svc.Env,
		Headers:        svc.Headers,
		WorkingDir:     svc.WorkingDir,
		HealthCheck:    svc.HealthCheck,
		StartupTimeout: svc.StartupTimeout.String(),
		Platform:       svc.Platform,
		Actions:        svc.Actions,
	})
	h.Write(data)
	return fmt.Sprintf("%x", h.Sum(nil))
}
