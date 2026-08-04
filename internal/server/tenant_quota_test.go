package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/budget"
	"smartrouter/internal/core"
)

// tenantQuotaCheckerStub implements BudgetChecker with a controllable
// CheckTenant result.
type tenantQuotaCheckerStub struct {
	tenantErr  error
	tenantCall int
	checkCall  int
}

func (s *tenantQuotaCheckerStub) Check(_ context.Context, _ string, _ time.Time) error {
	s.checkCall++
	return nil
}

func (s *tenantQuotaCheckerStub) CheckTenant(_ context.Context, _ time.Time) error {
	s.tenantCall++
	return s.tenantErr
}

func invokeTenantQuota(t *testing.T, checker BudgetChecker, withTenant bool) (*httptest.ResponseRecorder, *int) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if withTenant {
		req = req.WithContext(core.WithTenantID(req.Context(), "tenant-a"))
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	nextCalled := 0
	handler := TenantQuotaMiddleware(checker)(func(c *echo.Context) error {
		nextCalled++
		return c.String(http.StatusOK, "ok")
	})
	if err := handler(c); err != nil {
		t.Fatalf("middleware handler returned error: %v", err)
	}
	return rec, &nextCalled
}

func TestTenantQuotaMiddlewareSkipsWhenNoTenant(t *testing.T) {
	checker := &tenantQuotaCheckerStub{}
	rec, nextCalled := invokeTenantQuota(t, checker, false)

	if *nextCalled != 1 {
		t.Fatalf("next called %d times, want 1", *nextCalled)
	}
	if checker.tenantCall != 0 {
		t.Fatalf("CheckTenant called %d times, want 0 (no tenant)", checker.tenantCall)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestTenantQuotaMiddlewareAllowsWithinBudget(t *testing.T) {
	checker := &tenantQuotaCheckerStub{}
	rec, nextCalled := invokeTenantQuota(t, checker, true)

	if *nextCalled != 1 {
		t.Fatalf("next called %d times, want 1", *nextCalled)
	}
	if checker.tenantCall != 1 {
		t.Fatalf("CheckTenant called %d times, want 1", checker.tenantCall)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestTenantQuotaMiddlewareReturns402OnExceeded(t *testing.T) {
	checker := &tenantQuotaCheckerStub{
		tenantErr: &budget.ExceededError{
			Result: budget.CheckResult{
				Budget: budget.Budget{
					UserPath:      "/*",
					PeriodSeconds: budget.PeriodDailySeconds,
					Amount:        10,
				},
				PeriodEnd: time.Now().UTC().Add(time.Hour),
				Spent:     10,
			},
		},
	}
	rec, nextCalled := invokeTenantQuota(t, checker, true)

	if *nextCalled != 0 {
		t.Fatalf("next called %d times, want 0 (blocked)", *nextCalled)
	}
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusPaymentRequired)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"tenant_budget_exceeded"`) {
		t.Fatalf("body = %s, want tenant_budget_exceeded type", body)
	}
}

func TestTenantQuotaMiddlewareAllowsOnTransientError(t *testing.T) {
	checker := &tenantQuotaCheckerStub{tenantErr: errors.New("budget store unavailable")}
	rec, nextCalled := invokeTenantQuota(t, checker, true)

	if *nextCalled != 1 {
		t.Fatalf("next called %d times, want 1 (transient failure must not block)", *nextCalled)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestTenantQuotaMiddlewareNilCheckerIsNoop(t *testing.T) {
	rec, nextCalled := invokeTenantQuota(t, nil, true)

	if *nextCalled != 1 {
		t.Fatalf("next called %d times, want 1", *nextCalled)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
