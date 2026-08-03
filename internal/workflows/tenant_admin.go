package workflows

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ListActiveForTenant returns the active workflow versions persisted under
// tenantID, bypassing the process-wide compiled-workflow snapshot.
func (s *Service) ListActiveForTenant(ctx context.Context, tenantID string) ([]Version, error) {
	if s == nil {
		return []Version{}, nil
	}
	return s.store.ListActive(ctx, tenantID)
}

// ListEffectiveForTenant returns the effective active workflows visible to
// tenantID (platform-default fallback plus tenant-specific), bypassing the
// process-wide compiled-workflow snapshot.
func (s *Service) ListEffectiveForTenant(ctx context.Context, tenantID string) ([]Version, error) {
	if s == nil {
		return []Version{}, nil
	}
	return s.store.ListEffective(ctx, tenantID)
}

// GetForTenant returns one workflow version persisted under tenantID,
// bypassing the process-wide compiled-workflow snapshot.
func (s *Service) GetForTenant(ctx context.Context, tenantID, id string) (*Version, error) {
	if s == nil {
		return nil, fmt.Errorf("workflow service is required")
	}
	return s.store.Get(ctx, tenantID, strings.TrimSpace(id))
}

// CreateForTenant persists a new workflow version under tenantID. The
// shared compiled-workflow cache used by the live inference path is
// rebuilt only when tenantID is the platform-default tenant (s.tenantID);
// a non-default tenant's workflow does not affect live traffic until P5.
func (s *Service) CreateForTenant(ctx context.Context, tenantID string, input CreateInput) (*Version, error) {
	if s == nil {
		return nil, fmt.Errorf("workflow service is required")
	}
	normalized, _, _, err := normalizeCreateInput(input)
	if err != nil {
		return nil, err
	}
	version, err := s.store.Create(ctx, tenantID, normalized)
	if err != nil {
		return nil, err
	}
	if tenantID == s.tenantID {
		if err := s.Refresh(ctx); err != nil {
			return nil, err
		}
	}
	return version, nil
}

// DeactivateForTenant turns off one workflow version persisted under
// tenantID, bypassing the process-wide compiled-workflow snapshot unless
// tenantID is the platform-default tenant.
func (s *Service) DeactivateForTenant(ctx context.Context, tenantID, id string) error {
	if s == nil {
		return fmt.Errorf("workflow service is required")
	}
	version, err := s.store.Get(ctx, tenantID, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("load workflow %q: %w", id, err)
	}
	if version == nil {
		return ErrNotFound
	}
	if err := s.store.Deactivate(ctx, tenantID, version.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("deactivate workflow %q: %w", version.ID, err)
	}
	if tenantID == s.tenantID {
		return s.Refresh(ctx)
	}
	return nil
}
