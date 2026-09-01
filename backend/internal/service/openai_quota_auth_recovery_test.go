package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type rotatingQuotaAccountRepo struct {
	AccountRepository
	mu      sync.RWMutex
	account *Account
}

func (r *rotatingQuotaAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.account == nil || r.account.ID != id {
		return nil, fmt.Errorf("account %d not found", id)
	}
	return cloneQuotaAuthAccount(r.account), nil
}

func (r *rotatingQuotaAccountRepo) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.account == nil || r.account.ID != id {
		return fmt.Errorf("account %d not found", id)
	}
	r.account.Credentials = shallowCopyMap(credentials)
	return nil
}

func (r *rotatingQuotaAccountRepo) replace(account *Account) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.account = cloneQuotaAuthAccount(account)
}

func cloneQuotaAuthAccount(account *Account) *Account {
	if account == nil {
		return nil
	}
	cloned := *account
	cloned.Credentials = shallowCopyMap(account.Credentials)
	return &cloned
}

type quotaAuthTokenCache struct {
	mu     sync.Mutex
	tokens map[string]string
}

func (c *quotaAuthTokenCache) GetAccessToken(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if token := c.tokens[key]; token != "" {
		return token, nil
	}
	return "", fmt.Errorf("token %q not found", key)
}

func (c *quotaAuthTokenCache) SetAccessToken(_ context.Context, key, token string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens = make(map[string]string)
	}
	c.tokens[key] = token
	return nil
}

func (c *quotaAuthTokenCache) DeleteAccessToken(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.tokens, key)
	return nil
}

func (c *quotaAuthTokenCache) AcquireRefreshLock(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *quotaAuthTokenCache) ReleaseRefreshLock(_ context.Context, _ string) error {
	return nil
}

type quotaAuthRefreshExecutor struct {
	refreshCalls int
	accessToken  string
}

func (e *quotaAuthRefreshExecutor) CanRefresh(_ *Account) bool {
	return true
}

func (e *quotaAuthRefreshExecutor) NeedsRefresh(_ *Account, _ time.Duration) bool {
	return true
}

func (e *quotaAuthRefreshExecutor) Refresh(_ context.Context, account *Account) (map[string]any, error) {
	e.refreshCalls++
	credentials := shallowCopyMap(account.Credentials)
	credentials["access_token"] = e.accessToken
	credentials["expires_at"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	return credentials, nil
}

func TestQueryUsageOAuth401ForcesSharedRefreshOnce(t *testing.T) {
	account := newQuotaAuthOAuthAccount("rejected-token", "chatgpt-account", "user-id", 1)
	repo := &rotatingQuotaAccountRepo{account: account}
	cache := &quotaAuthTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "rejected-token"}}
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		require.Equal(t, "Bearer refreshed-token", r.Header.Get("Authorization"))
		writeQuotaUsageSuccess(w, "chatgpt-account", "user-id")
	}))
	t.Cleanup(srv.Close)

	svc, provider := newQuotaAuthService(repo, cache, srv)
	executor := &quotaAuthRefreshExecutor{accessToken: "refreshed-token"}
	provider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), executor)
	usage, err := svc.QueryUsageSnapshot(context.Background(), account.ID)

	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 1, executor.refreshCalls)
	require.EqualValues(t, 2, atomic.LoadInt32(&calls))
}

func (e *quotaAuthRefreshExecutor) CacheKey(account *Account) string {
	return OpenAITokenCacheKey(account)
}

func newQuotaAuthOAuthAccount(accessToken, chatGPTAccountID, chatGPTUserID string, tokenVersion int64) *Account {
	return &Account{
		ID:       100,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Status:   StatusActive,
		Credentials: map[string]any{
			"access_token":       accessToken,
			"refresh_token":      "refresh-token",
			"expires_at":         time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"chatgpt_account_id": chatGPTAccountID,
			"chatgpt_user_id":    chatGPTUserID,
			"plan_type":          "pro",
			"_token_version":     tokenVersion,
		},
	}
}

func newQuotaAuthService(
	repo AccountRepository,
	cache OpenAITokenCache,
	srv *httptest.Server,
) (*OpenAIQuotaService, *OpenAITokenProvider) {
	tokenProvider := NewOpenAITokenProvider(repo, cache, nil)
	return NewOpenAIQuotaService(repo, nil, tokenProvider, newQuotaRedirectingFactory(srv)), tokenProvider
}

func writeQuotaUsageSuccess(w http.ResponseWriter, accountID, userID string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"account_id":%q,"user_id":%q,"plan_type":"pro","rate_limit":{"allowed":true}}`, accountID, userID)
}

func TestQueryUsageOAuth401RereadsRotatedCredentialsAndRetriesOnce(t *testing.T) {
	initial := newQuotaAuthOAuthAccount("stale-access-token", "chatgpt-old", "user-old", 1)
	rotated := newQuotaAuthOAuthAccount("rotated-access-token", "chatgpt-new", "user-new", 2)
	repo := &rotatingQuotaAccountRepo{account: initial}
	cache := &quotaAuthTokenCache{tokens: map[string]string{
		OpenAITokenCacheKey(initial): "stale-access-token",
	}}

	var calls int32
	var headersMu sync.Mutex
	var authorizations []string
	var accountIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headersMu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		accountIDs = append(accountIDs, r.Header.Get("chatgpt-account-id"))
		headersMu.Unlock()

		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			// 模拟首次 401 到达时，管理员已在数据库中完成重新授权。
			repo.replace(rotated)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"expired upstream credential"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer rotated-access-token" ||
			r.Header.Get("chatgpt-account-id") != "chatgpt-new" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeQuotaUsageSuccess(w, "chatgpt-new", "user-new")
	}))
	t.Cleanup(srv.Close)

	svc, _ := newQuotaAuthService(repo, cache, srv)
	usage, err := svc.QueryUsageSnapshot(context.Background(), initial.ID)

	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, "chatgpt-new", usage.AccountID)
	require.EqualValues(t, 2, atomic.LoadInt32(&calls), "首次 401 后最多只允许重试一次")
	headersMu.Lock()
	defer headersMu.Unlock()
	require.Equal(t, []string{"Bearer stale-access-token", "Bearer rotated-access-token"}, authorizations)
	require.Equal(t, []string{"chatgpt-old", "chatgpt-new"}, accountIDs)
}

func TestQueryUsagePAT401DoesNotRefreshAndRequiresReauthorization(t *testing.T) {
	account := newQuotaAuthOAuthAccount("pat-token", "chatgpt-pat", "user-pat", 1)
	account.Credentials[openAIAuthModeCredentialKey] = OpenAIAuthModePersonalAccessToken
	repo := &rotatingQuotaAccountRepo{account: account}
	cache := &quotaAuthTokenCache{tokens: map[string]string{
		OpenAITokenCacheKey(account): "pat-token",
	}}

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"PAT rejected"}`))
	}))
	t.Cleanup(srv.Close)

	svc, tokenProvider := newQuotaAuthService(repo, cache, srv)
	refreshExecutor := &quotaAuthRefreshExecutor{}
	tokenProvider.SetRefreshAPI(NewOAuthRefreshAPI(repo, cache), refreshExecutor)

	usage, err := svc.QueryUsageSnapshot(context.Background(), account.ID)

	require.Nil(t, usage)
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, infraerrors.Code(err))
	require.Equal(t, "OPENAI_QUOTA_REAUTH_REQUIRED", infraerrors.Reason(err))
	require.Zero(t, refreshExecutor.refreshCalls, "PAT 被拒绝后不得执行 OAuth refresh")
	require.Positive(t, atomic.LoadInt32(&calls))
}

func TestMapUpstreamStatusDoesNotExposeUpstreamUnauthorized(t *testing.T) {
	require.Equal(t, http.StatusBadGateway, mapUpstreamStatus(http.StatusUnauthorized))
}

func TestQueryUsageRetriesTransientUpstreamStatusOnce(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			account := newQuotaAuthOAuthAccount("access-token", "chatgpt-account", "user-id", 1)
			repo := &rotatingQuotaAccountRepo{account: account}
			cache := &quotaAuthTokenCache{tokens: map[string]string{
				OpenAITokenCacheKey(account): "access-token",
			}}
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if atomic.AddInt32(&calls, 1) == 1 {
					w.WriteHeader(status)
					return
				}
				writeQuotaUsageSuccess(w, "chatgpt-account", "user-id")
			}))
			t.Cleanup(srv.Close)

			svc, _ := newQuotaAuthService(repo, cache, srv)
			usage, err := svc.QueryUsageSnapshot(context.Background(), account.ID)

			require.NoError(t, err)
			require.NotNil(t, usage)
			require.EqualValues(t, 2, atomic.LoadInt32(&calls), "临时上游错误应只重试一次")
		})
	}
}

func TestQueryUsageStopsAfterOneTransientRetry(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		wantReason string
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, wantReason: "OPENAI_QUOTA_RATE_LIMITED"},
		{name: "temporary upstream failure", status: http.StatusServiceUnavailable, wantReason: "OPENAI_QUOTA_UPSTREAM_ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := newQuotaAuthOAuthAccount("access-token", "chatgpt-account", "user-id", 1)
			repo := &rotatingQuotaAccountRepo{account: account}
			cache := &quotaAuthTokenCache{tokens: map[string]string{
				OpenAITokenCacheKey(account): "access-token",
			}}
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(srv.Close)

			svc, _ := newQuotaAuthService(repo, cache, srv)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			usage, err := svc.QueryUsageSnapshot(ctx, account.ID)

			require.Nil(t, usage)
			require.Error(t, err)
			require.Equal(t, tt.wantReason, infraerrors.Reason(err))
			require.EqualValues(t, 2, atomic.LoadInt32(&calls), "持续临时错误也不得重试超过一次")
		})
	}
}

func TestQueryUsageRejectsResponseAfterCredentialIdentityChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Account)
	}{
		{
			name: "token version changed",
			mutate: func(account *Account) {
				account.Credentials["_token_version"] = int64(2)
				account.Credentials["access_token"] = "rotated-access-token"
			},
		},
		{
			name: "upstream identity changed",
			mutate: func(account *Account) {
				account.Credentials["chatgpt_account_id"] = "chatgpt-new"
				account.Credentials["chatgpt_user_id"] = "user-new"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initial := newQuotaAuthOAuthAccount("access-token", "chatgpt-old", "user-old", 1)
			repo := &rotatingQuotaAccountRepo{account: initial}
			cache := &quotaAuthTokenCache{tokens: map[string]string{
				OpenAITokenCacheKey(initial): "access-token",
			}}
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&calls, 1)
				changed := cloneQuotaAuthAccount(initial)
				tt.mutate(changed)
				repo.replace(changed)
				writeQuotaUsageSuccess(w, "chatgpt-old", "user-old")
			}))
			t.Cleanup(srv.Close)

			svc, _ := newQuotaAuthService(repo, cache, srv)
			usage, err := svc.QueryUsageSnapshot(context.Background(), initial.ID)

			require.Nil(t, usage)
			require.Error(t, err)
			require.Equal(t, http.StatusConflict, infraerrors.Code(err))
			require.Equal(t, "OPENAI_QUOTA_STALE_CREDENTIAL_RESPONSE", infraerrors.Reason(err))
			require.EqualValues(t, 1, atomic.LoadInt32(&calls))
		})
	}
}
