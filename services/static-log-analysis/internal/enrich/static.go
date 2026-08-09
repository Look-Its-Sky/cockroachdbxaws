package enrich

import (
	"context"
	"fmt"
	"strings"
)

// Static is a Provider backed by operator configuration rather than by an
// external system of record.
//
// It exists because many deployments already know their release identity at
// deploy time and have no metadata service to ask. It is a real provider, not a
// test double: it answers ErrNoDeployment for a service it was not told about,
// which is a definite negative rather than a failure, so an unlisted service
// groups under unknown-deployment instead of retrying forever.
type Static struct {
	// byService is keyed on service and environment, because one service may run
	// different releases in different environments.
	byService map[staticKey]Deployment
}

type staticKey struct{ service, environment string }

// NewStatic builds a provider from declared entries. Each entry is
// service=environment=deployment_id=version.
func NewStatic(entries []string) (*Static, error) {
	byService := make(map[staticKey]Deployment, len(entries))
	for _, entry := range entries {
		parts := strings.Split(entry, "=")
		if len(parts) != 4 {
			return nil, fmt.Errorf("%w: deployment entry %q must be service=environment=deployment_id=version",
				ErrInvalidConfig, entry)
		}
		for _, part := range parts {
			if part == "" || strings.TrimSpace(part) != part {
				return nil, fmt.Errorf("%w: deployment entry %q has an empty or padded component",
					ErrInvalidConfig, entry)
			}
		}
		key := staticKey{service: parts[0], environment: parts[1]}
		if _, exists := byService[key]; exists {
			// Two entries for one service and environment would give it two
			// identities, and which one a record got would depend on map order.
			return nil, fmt.Errorf("%w: service %q in environment %q is declared twice",
				ErrInvalidConfig, key.service, key.environment)
		}
		byService[key] = Deployment{ID: parts[2], Version: parts[3]}
	}
	return &Static{byService: byService}, nil
}

// Deployment answers from the declared table.
func (s *Static) Deployment(_ context.Context, key Key) (Deployment, error) {
	if s == nil {
		return Deployment{}, ErrNoDeployment
	}
	if found, ok := s.byService[staticKey{service: key.Service, environment: key.Environment}]; ok {
		return found, nil
	}
	// Definite: this provider knows everything it is ever going to know, so an
	// absence here is an absence rather than an outage.
	return Deployment{}, ErrNoDeployment
}

// Len is the number of declared deployments.
func (s *Static) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byService)
}
