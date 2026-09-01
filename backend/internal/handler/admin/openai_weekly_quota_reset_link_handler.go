package admin

import (
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type OpenAIWeeklyQuotaResetLinkHandler struct {
	service *service.OpenAIWeeklyQuotaResetLinkService
}

func NewOpenAIWeeklyQuotaResetLinkHandler(s *service.OpenAIWeeklyQuotaResetLinkService) *OpenAIWeeklyQuotaResetLinkHandler {
	return &OpenAIWeeklyQuotaResetLinkHandler{service: s}
}

func (h *OpenAIWeeklyQuotaResetLinkHandler) ListRules(c *gin.Context) {
	items, err := h.service.ListRules(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *OpenAIWeeklyQuotaResetLinkHandler) ListSourceAccounts(c *gin.Context) {
	items, err := h.service.ListSourceAccounts(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *OpenAIWeeklyQuotaResetLinkHandler) CreateRule(c *gin.Context) {
	var input service.OpenAIWeeklyQuotaResetRuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid request body"))
		return
	}
	item, err := h.service.CreateRule(c.Request.Context(), input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *OpenAIWeeklyQuotaResetLinkHandler) UpdateRule(c *gin.Context) {
	id, ok := parseWeeklyQuotaResetRuleID(c)
	if !ok {
		return
	}
	var input service.OpenAIWeeklyQuotaResetRuleInput
	if err := c.ShouldBindJSON(&input); err != nil {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_REQUEST", "invalid request body"))
		return
	}
	item, err := h.service.UpdateRule(c.Request.Context(), id, input)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *OpenAIWeeklyQuotaResetLinkHandler) DeleteRule(c *gin.Context) {
	id, ok := parseWeeklyQuotaResetRuleID(c)
	if !ok {
		return
	}
	if err := h.service.DeleteRule(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *OpenAIWeeklyQuotaResetLinkHandler) CheckRule(c *gin.Context) {
	id, ok := parseWeeklyQuotaResetRuleID(c)
	if !ok {
		return
	}
	result, err := h.service.CheckRuleNow(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *OpenAIWeeklyQuotaResetLinkHandler) ListExecutions(c *gin.Context) {
	var ruleID *int64
	if raw := strings.TrimSpace(c.Query("rule_id")); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			response.ErrorFrom(c, infraerrors.BadRequest("INVALID_RULE_ID", "invalid rule id"))
			return
		}
		ruleID = &id
	}
	limit := 50
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 200 {
			response.ErrorFrom(c, infraerrors.BadRequest("INVALID_LIMIT", "limit must be between 1 and 200"))
			return
		}
		limit = parsed
	}
	items, err := h.service.ListExecutions(c.Request.Context(), ruleID, limit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *OpenAIWeeklyQuotaResetLinkHandler) ListResetEvents(c *gin.Context) {
	limit := 50
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 200 {
			response.ErrorFrom(c, infraerrors.BadRequest("INVALID_LIMIT", "limit must be between 1 and 200"))
			return
		}
		limit = parsed
	}
	items, err := h.service.ListResetEvents(c.Request.Context(), limit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func parseWeeklyQuotaResetRuleID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorFrom(c, infraerrors.BadRequest("INVALID_RULE_ID", "invalid rule id"))
		return 0, false
	}
	return id, true
}
