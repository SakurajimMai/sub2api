package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSelectOpenAIWeeklyWindow(t *testing.T) {
	t.Parallel()

	const day = int64((24 * time.Hour) / time.Second)
	tests := []struct {
		name       string
		usage      *OpenAIQuotaUsage
		wantOK     bool
		wantReset  int64
		wantStart  int64
		wantWindow int64
	}{
		{
			name: "secondary seven day window",
			usage: &OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{
				PrimaryWindow:   &OpenAIRateLimitWindow{LimitWindowSeconds: 5 * 60 * 60, ResetAt: 100},
				SecondaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 7 * day, ResetAt: 1_800_000},
			}},
			wantOK:     true,
			wantReset:  1_800_000,
			wantStart:  1_800_000 - 7*day,
			wantWindow: 7 * day,
		},
		{
			name: "primary weekly window",
			usage: &OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{
				PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 6 * day, ResetAt: 2_000_000},
			}},
			wantOK:     true,
			wantReset:  2_000_000,
			wantStart:  2_000_000 - 6*day,
			wantWindow: 6 * day,
		},
		{
			name: "closest candidate wins",
			usage: &OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{
				PrimaryWindow:   &OpenAIRateLimitWindow{LimitWindowSeconds: 8 * day, ResetAt: 3_000_000},
				SecondaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 7 * day, ResetAt: 4_000_000},
			}},
			wantOK:     true,
			wantReset:  4_000_000,
			wantStart:  4_000_000 - 7*day,
			wantWindow: 7 * day,
		},
		{
			name: "missing reset timestamp",
			usage: &OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{
				SecondaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 7 * day},
			}},
		},
		{
			name: "outside weekly range",
			usage: &OpenAIQuotaUsage{RateLimit: &OpenAIRateLimit{
				PrimaryWindow:   &OpenAIRateLimitWindow{LimitWindowSeconds: 5 * 60 * 60, ResetAt: 100},
				SecondaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 9 * day, ResetAt: 200},
			}},
		},
		{name: "missing rate limit", usage: &OpenAIQuotaUsage{}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := selectOpenAIWeeklyWindow(tt.usage)
			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}
			require.Equal(t, tt.wantReset, got.ResetAt.Unix())
			require.Equal(t, tt.wantStart, got.WindowStart.Unix())
			require.Equal(t, tt.wantWindow, got.WindowSeconds)
		})
	}
}

func TestQueryUsageSnapshotSkipsResetCreditEndpoint(t *testing.T) {
	account := &Account{
		ID:       710,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "org-weekly-link",
		},
	}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{account.ID: account}}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{
		OpenAITokenCacheKey(account): "weekly-link-token",
	}}

	usageCalls := 0
	creditCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if strings.Contains(r.URL.Path, "rate-limit-reset-credits") {
			creditCalls++
			_, _ = w.Write([]byte(`{"available_count":1}`))
			return
		}
		usageCalls++
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"secondary_window":{"limit_window_seconds":604800,"reset_at":1800000}}}`))
	}))
	defer server.Close()

	service := NewOpenAIQuotaService(
		repo,
		nil,
		NewOpenAITokenProvider(repo, tokenCache, nil),
		newQuotaRedirectingFactory(server),
	)
	usage, err := service.QueryUsageSnapshot(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 1, usageCalls)
	require.Zero(t, creditCalls)
}
