package gateway

import (
	"context"
	"strings"

	"smartrouter/internal/core"
)

type modelCountProvider interface {
	ModelCount() int
}

type providerModelRefresher interface {
	RefreshProviderModels(ctx context.Context, providerSelector string) (int, error)
}

type modelRefreshTargetResolver interface {
	ResolveRefreshTarget(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error)
}

// ResolvedProviderName returns the configured provider instance name for a selector.
func ResolvedProviderName(provider core.RoutableProvider, selector core.ModelSelector, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if provider == nil {
		return fallback
	}
	if named, ok := provider.(core.ProviderNameResolver); ok {
		if providerName := strings.TrimSpace(named.GetProviderName(selector.QualifiedModel())); providerName != "" {
			return providerName
		}
	}
	return fallback
}

// ResolvedWorkflowProviderName returns the configured provider name recorded in a resolution.
func ResolvedWorkflowProviderName(resolution *core.RequestModelResolution) string {
	if resolution == nil {
		return ""
	}
	if providerName := strings.TrimSpace(resolution.ProviderName); providerName != "" {
		return providerName
	}
	return strings.TrimSpace(resolution.ResolvedSelector.Provider)
}

// WorkflowProviderNameForType maps a provider type to its configured provider name when available.
func WorkflowProviderNameForType(provider core.RoutableProvider, providerType string) string {
	providerType = strings.TrimSpace(providerType)
	if providerType == "" || provider == nil {
		return ""
	}
	if named, ok := provider.(core.ProviderTypeNameResolver); ok {
		return strings.TrimSpace(named.GetProviderNameForType(providerType))
	}
	return ""
}

// ResolveRequestModel resolves a requested selector into a concrete provider/model selector.
func ResolveRequestModel(provider core.RoutableProvider, resolver ModelResolver, requested core.RequestedModelSelector) (*core.RequestModelResolution, error) {
	return ResolveRequestModelWithAuthorizer(context.Background(), provider, resolver, nil, requested)
}

// ResolveRequestModelWithAuthorizer resolves and validates a requested selector.
func ResolveRequestModelWithAuthorizer(
	ctx context.Context,
	provider core.RoutableProvider,
	resolver ModelResolver,
	authorizer ModelAuthorizer,
	requested core.RequestedModelSelector,
) (*core.RequestModelResolution, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requested = core.NewRequestedModelSelector(requested.Model, requested.ProviderHint)

	resolvedSelector, aliasApplied, labels, err := ResolveExecutionSelector(ctx, provider, resolver, requested)
	refreshed := false
	if err != nil {
		var refreshErr error
		refreshed, refreshErr = refreshProviderModelsForResolution(ctx, provider, resolver, requested, resolvedSelector)
		if refreshErr != nil {
			return nil, refreshErr
		}
		if !refreshed {
			return nil, core.NewInvalidRequestError(err.Error(), err)
		}
		resolvedSelector, aliasApplied, labels, err = ResolveExecutionSelector(ctx, provider, resolver, requested)
		if err != nil {
			return nil, core.NewInvalidRequestError(err.Error(), err)
		}
	}
	if resolvedSelector == (core.ModelSelector{}) {
		resolvedSelector, err = requested.Normalize()
		if err != nil {
			return nil, core.NewInvalidRequestError(err.Error(), err)
		}
	}

	resolvedModel := resolvedSelector.QualifiedModel()
	if counted, ok := provider.(modelCountProvider); ok && counted.ModelCount() == 0 {
		if !refreshed {
			var refreshErr error
			refreshed, refreshErr = refreshProviderModelsForResolution(ctx, provider, resolver, requested, resolvedSelector)
			if refreshErr != nil {
				return nil, refreshErr
			}
			if refreshed {
				resolvedSelector, aliasApplied, labels, err = ResolveExecutionSelector(ctx, provider, resolver, requested)
				if err != nil {
					return nil, core.NewInvalidRequestError(err.Error(), err)
				}
				resolvedModel = resolvedSelector.QualifiedModel()
			}
		}
	}
	if counted, ok := provider.(modelCountProvider); ok && counted.ModelCount() == 0 {
		return nil, core.NewProviderError("", 0, "model registry not initialized", nil)
	}
	if !provider.Supports(resolvedModel) {
		if !refreshed {
			var refreshErr error
			refreshed, refreshErr = refreshProviderModelsForResolution(ctx, provider, resolver, requested, resolvedSelector)
			if refreshErr != nil {
				return nil, refreshErr
			}
			if refreshed {
				resolvedSelector, aliasApplied, labels, err = ResolveExecutionSelector(ctx, provider, resolver, requested)
				if err != nil {
					return nil, core.NewInvalidRequestError(err.Error(), err)
				}
				resolvedModel = resolvedSelector.QualifiedModel()
			}
		}
	}
	if !provider.Supports(resolvedModel) {
		return nil, core.NewModelNotFoundError(resolvedModel)
	}
	if authorizer != nil {
		if err := authorizer.ValidateModelAccess(ctx, resolvedSelector); err != nil {
			return nil, err
		}
	}

	return &core.RequestModelResolution{
		Requested:        requested,
		ResolvedSelector: resolvedSelector,
		ProviderType:     strings.TrimSpace(provider.GetProviderType(resolvedModel)),
		ProviderName:     ResolvedProviderName(provider, resolvedSelector, ""),
		AliasApplied:     aliasApplied,
		Labels:           labels,
	}, nil
}

func refreshProviderModelsForResolution(
	ctx context.Context,
	provider core.RoutableProvider,
	resolver ModelResolver,
	requested core.RequestedModelSelector,
	resolvedSelector core.ModelSelector,
) (bool, error) {
	refresher, ok := provider.(providerModelRefresher)
	if !ok {
		return false, nil
	}

	providerSelector := strings.TrimSpace(resolvedSelector.Provider)
	if providerSelector == "" {
		if targetResolver, ok := resolver.(modelRefreshTargetResolver); ok {
			selector, ok, err := targetResolver.ResolveRefreshTarget(ctx, requested)
			if err != nil {
				return false, err
			}
			if ok {
				providerSelector = strings.TrimSpace(selector.Provider)
			}
		}
	}
	if providerSelector == "" {
		selector, err := requested.Normalize()
		if err != nil {
			return false, nil
		}
		providerSelector = strings.TrimSpace(selector.Provider)
	}
	if providerSelector == "" {
		return false, nil
	}

	_, err := refresher.RefreshProviderModels(ctx, providerSelector)
	return true, err
}

// ResolveExecutionSelector applies explicit and provider-owned selector
// resolution. ctx carries the effective request user path so a resolver that
// implements UserPathModelResolver can apply user_path-scoped redirects.
func ResolveExecutionSelector(
	ctx context.Context,
	provider core.RoutableProvider,
	resolver ModelResolver,
	requested core.RequestedModelSelector,
) (core.ModelSelector, bool, []string, error) {
	requested = core.NewRequestedModelSelector(requested.Model, requested.ProviderHint)

	var (
		resolvedSelector core.ModelSelector
		aliasApplied     bool
		labels           []string
		err              error
	)

	if resolver != nil {
		resolvedSelector, aliasApplied, labels, err = resolveRequestedModel(ctx, resolver, requested)
		if err != nil {
			return core.ModelSelector{}, false, nil, err
		}
		requested = core.NewRequestedModelSelector(resolvedSelector.QualifiedModel(), "")
	}

	if providerResolver, ok := provider.(ModelResolver); ok {
		providerSelector, providerChanged, err := providerResolver.ResolveModel(ctx, requested)
		if err != nil {
			if resolvedSelector != (core.ModelSelector{}) {
				// Preserve alias targets so callers can refresh the concrete provider before retrying.
				return resolvedSelector, aliasApplied, labels, err
			}
			return core.ModelSelector{}, false, nil, err
		}
		return providerSelector, aliasApplied || providerChanged, labels, nil
	}

	if resolvedSelector != (core.ModelSelector{}) {
		return resolvedSelector, aliasApplied, labels, nil
	}

	resolvedSelector, err = requested.Normalize()
	return resolvedSelector, aliasApplied, labels, err
}

// resolveRequestedModel resolves through a LabelingModelResolver when the
// resolver implements it (capturing any observability labels it attaches),
// else through a UserPathModelResolver (applying user_path-scoped redirects),
// falling back to the unscoped ModelResolver otherwise.
func resolveRequestedModel(ctx context.Context, resolver ModelResolver, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error) {
	if labeling, ok := resolver.(LabelingModelResolver); ok {
		return labeling.ResolveModelWithLabels(ctx, requested)
	}
	if scoped, ok := resolver.(UserPathModelResolver); ok {
		selector, changed, err := scoped.ResolveModelForUserPath(ctx, requested)
		return selector, changed, nil, err
	}
	selector, changed, err := resolver.ResolveModel(ctx, requested)
	return selector, changed, nil, err
}
