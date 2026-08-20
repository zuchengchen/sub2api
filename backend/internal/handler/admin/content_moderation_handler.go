package admin

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type ContentModerationHandler struct {
	service *service.ContentModerationService
}

func NewContentModerationHandler(svc *service.ContentModerationService) *ContentModerationHandler {
	return &ContentModerationHandler{service: svc}
}

// StartHealthWorkers is called by router construction, after all Wire
// providers that can fail have initialized. The workers themselves are
// asynchronous and do not delay HTTP serving.
func (h *ContentModerationHandler) StartHealthWorkers() {
	if h != nil && h.service != nil {
		h.service.Start()
	}
}

type contentModerationConfigRequest struct {
	Enabled                 *bool                                            `json:"enabled"`
	Mode                    *string                                          `json:"mode"`
	DeepSeekEnabled         *bool                                            `json:"deepseek_enabled"`
	RemoteReviewersEnabled  *bool                                            `json:"remote_reviewers_enabled"`
	RemoteConsensusRequired *int                                             `json:"remote_consensus_required"`
	RemoteUnavailablePolicy *string                                          `json:"remote_unavailable_policy"`
	YuFengEnabled           *bool                                            `json:"yufeng_enabled"`
	YuFengMode              *string                                          `json:"yufeng_mode"`
	DeepSeekTotalTimeoutMS  *int                                             `json:"deepseek_total_timeout_ms"`
	DeepSeekThreshold       *float64                                         `json:"deepseek_threshold"`
	DeepSeekChannels        *[]service.ContentModerationDeepSeekChannelInput `json:"deepseek_channels"`
	RemoteReviewers         *[]service.ContentModerationDeepSeekChannelInput `json:"remote_reviewers"`
	AllGroups               *bool                                            `json:"all_groups"`
	GroupIDs                *[]int64                                         `json:"group_ids"`
	UserEmailWhitelist      *[]string                                        `json:"user_email_whitelist"`
	RecordNonHits           *bool                                            `json:"record_non_hits"`
	BlockStatus             *int                                             `json:"block_status"`
	BlockMessage            *string                                          `json:"block_message"`
	EmailOnHit              *bool                                            `json:"email_on_hit"`
	AutoBanEnabled          *bool                                            `json:"auto_ban_enabled"`
	BanThreshold            *int                                             `json:"ban_threshold"`
	ViolationWindowHours    *int                                             `json:"violation_window_hours"`
	// cyber_policy 命中是否排除出自动封号计数；前端 RiskControlView 已发送该字段，
	// service.UpdateContentModerationConfigInput 已支持，此前 handler 层缺透传导致开关静默失效。
	CyberPolicyExcludeFromBanCount *bool                                 `json:"cyber_policy_exclude_from_ban_count"`
	HitRetentionDays               *int                                  `json:"hit_retention_days"`
	NonHitRetentionDays            *int                                  `json:"non_hit_retention_days"`
	PreHashCheckEnabled            *bool                                 `json:"pre_hash_check_enabled"`
	BlockedKeywords                *[]string                             `json:"blocked_keywords"`
	ModelFilter                    *service.ContentModerationModelFilter `json:"model_filter"`
	CacheVersion                   *string                               `json:"cache_version"`
	CacheMaxEntries                *int                                  `json:"cache_max_entries"`
	CacheMaxBytes                  *int64                                `json:"cache_max_bytes"`
	FragmentBlockTTLSeconds        *int                                  `json:"fragment_block_ttl_seconds"`
	FragmentAllowTTLSeconds        *int                                  `json:"fragment_allow_ttl_seconds"`
	FragmentTTLPolicyVersion       *string                               `json:"fragment_ttl_policy_version"`
	FirstLayerStage                *string                               `json:"first_layer_stage"`
	SecondLayerEnabled             *bool                                 `json:"second_layer_enabled"`
	SecondLayerStage               *string                               `json:"second_layer_stage"`
	HardBlockPatterns              *[]string                             `json:"hard_block_patterns"`
	CandidateKeywords              *[]string                             `json:"candidate_keywords"`
	Layer1Keywords                 *[]string                             `json:"layer1_keywords"`
	Layer2Keywords                 *[]string                             `json:"layer2_keywords"`
	KeywordAllowlist               *[]string                             `json:"keyword_allowlist"`
	KeywordPolicyVersion           *string                               `json:"keyword_policy_version"`
	ContextPolicyVersion           *string                               `json:"context_policy_version"`
	EvidencePolicyVersion          *string                               `json:"evidence_policy_version"`
}

type contentModerationHashRequest struct {
	InputHash string `json:"input_hash"`
}

func (h *ContentModerationHandler) GetConfig(c *gin.Context) {
	cfg, err := h.service.GetConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

func (h *ContentModerationHandler) UpdateConfig(c *gin.Context) {
	var req contentModerationConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	cfg, err := h.service.UpdateConfig(c.Request.Context(), service.UpdateContentModerationConfigInput{
		Enabled:                        req.Enabled,
		Mode:                           req.Mode,
		DeepSeekEnabled:                req.DeepSeekEnabled,
		RemoteReviewersEnabled:         req.RemoteReviewersEnabled,
		RemoteConsensusRequired:        req.RemoteConsensusRequired,
		RemoteUnavailablePolicy:        req.RemoteUnavailablePolicy,
		YuFengEnabled:                  req.YuFengEnabled,
		YuFengMode:                     req.YuFengMode,
		DeepSeekTotalTimeoutMS:         req.DeepSeekTotalTimeoutMS,
		DeepSeekThreshold:              req.DeepSeekThreshold,
		DeepSeekChannels:               req.DeepSeekChannels,
		RemoteReviewers:                req.RemoteReviewers,
		AllGroups:                      req.AllGroups,
		GroupIDs:                       req.GroupIDs,
		UserEmailWhitelist:             req.UserEmailWhitelist,
		RecordNonHits:                  req.RecordNonHits,
		BlockStatus:                    req.BlockStatus,
		BlockMessage:                   req.BlockMessage,
		EmailOnHit:                     req.EmailOnHit,
		AutoBanEnabled:                 req.AutoBanEnabled,
		BanThreshold:                   req.BanThreshold,
		ViolationWindowHours:           req.ViolationWindowHours,
		CyberPolicyExcludeFromBanCount: req.CyberPolicyExcludeFromBanCount,
		HitRetentionDays:               req.HitRetentionDays,
		NonHitRetentionDays:            req.NonHitRetentionDays,
		PreHashCheckEnabled:            req.PreHashCheckEnabled,
		BlockedKeywords:                req.BlockedKeywords,
		ModelFilter:                    req.ModelFilter,
		CacheVersion:                   req.CacheVersion,
		CacheMaxEntries:                req.CacheMaxEntries,
		CacheMaxBytes:                  req.CacheMaxBytes,
		FragmentBlockTTLSeconds:        req.FragmentBlockTTLSeconds,
		FragmentAllowTTLSeconds:        req.FragmentAllowTTLSeconds,
		FragmentTTLPolicyVersion:       req.FragmentTTLPolicyVersion,
		FirstLayerStage:                req.FirstLayerStage,
		SecondLayerEnabled:             req.SecondLayerEnabled,
		SecondLayerStage:               req.SecondLayerStage,
		HardBlockPatterns:              req.HardBlockPatterns,
		CandidateKeywords:              req.CandidateKeywords,
		Layer1Keywords:                 req.Layer1Keywords,
		Layer2Keywords:                 req.Layer2Keywords,
		KeywordAllowlist:               req.KeywordAllowlist,
		KeywordPolicyVersion:           req.KeywordPolicyVersion,
		ContextPolicyVersion:           req.ContextPolicyVersion,
		EvidencePolicyVersion:          req.EvidencePolicyVersion,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

func (h *ContentModerationHandler) TestDeepSeekChannel(c *gin.Context) {
	result, err := h.service.TestDeepSeekChannel(c.Request.Context(), c.Param("id"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *ContentModerationHandler) TestContentModerationChannelAPI(c *gin.Context) {
	result, err := h.service.TestContentModerationChannelAPI(c.Request.Context(), c.Param("id"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *ContentModerationHandler) GetStatus(c *gin.Context) {
	status, err := h.service.GetStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

func (h *ContentModerationHandler) ListLogs(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	filter := service.ContentModerationLogFilter{
		Pagination: pagination.PaginationParams{
			Page:      page,
			PageSize:  pageSize,
			SortOrder: pagination.SortOrderDesc,
		},
		Result:         c.Query("result"),
		Endpoint:       c.Query("endpoint"),
		ContextClass:   c.Query("context_class"),
		ModelProfile:   c.Query("model_profile"),
		DecisionSource: c.Query("decision_source"),
		Search:         c.Query("search"),
	}
	if raw := strings.TrimSpace(c.Query("log_id")); raw != "" {
		logID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || logID <= 0 {
			response.BadRequest(c, "Invalid log_id")
			return
		}
		filter.LogID = &logID
	}
	if raw := strings.TrimSpace(c.Query("group_id")); raw != "" {
		groupID, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || groupID <= 0 {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		filter.GroupID = &groupID
	}
	if raw := strings.TrimSpace(c.Query("from")); raw != "" {
		t, _, err := parseContentModerationDate(raw)
		if err != nil {
			response.BadRequest(c, "Invalid from")
			return
		}
		filter.From = &t
	}
	if raw := strings.TrimSpace(c.Query("to")); raw != "" {
		t, dateOnly, err := parseContentModerationDate(raw)
		if err != nil {
			response.BadRequest(c, "Invalid to")
			return
		}
		if dateOnly {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		filter.To = &t
	}
	items, pageResult, err := h.service.ListLogs(c.Request.Context(), filter)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, pageResult.Total, pageResult.Page, pageResult.PageSize)
}

func (h *ContentModerationHandler) GetLog(c *gin.Context) {
	logID, ok := parseContentModerationLogID(c)
	if !ok {
		return
	}
	item, err := h.service.GetLog(c.Request.Context(), logID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *ContentModerationHandler) UnbanUser(c *gin.Context) {
	userID, err := strconv.ParseInt(strings.TrimSpace(c.Param("user_id")), 10, 64)
	if err != nil || userID <= 0 {
		response.BadRequest(c, "Invalid user_id")
		return
	}
	result, err := h.service.UnbanUser(c.Request.Context(), userID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *ContentModerationHandler) DeleteFlaggedHash(c *gin.Context) {
	var req contentModerationHashRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	result, err := h.service.DeleteFlaggedInputHash(c.Request.Context(), req.InputHash)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *ContentModerationHandler) ClearFlaggedHashes(c *gin.Context) {
	result, err := h.service.ClearFlaggedInputHashes(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *ContentModerationHandler) PreviewArchive(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	logID, ok := parseContentModerationLogID(c)
	if !ok {
		return
	}
	preview, err := h.service.PreviewArchive(c.Request.Context(), logID, contentModerationActorID(c), contentModerationAdminRequestID(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, preview)
}

func (h *ContentModerationHandler) DownloadArchive(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	logID, ok := parseContentModerationLogID(c)
	if !ok {
		return
	}
	raw, err := h.service.DownloadArchive(c.Request.Context(), logID, contentModerationActorID(c), contentModerationAdminRequestID(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=risk-archive-%d.json", logID))
	c.DataFromReader(http.StatusOK, int64(len(raw)), "application/json", bytes.NewReader(raw), nil)
}

func (h *ContentModerationHandler) DeleteArchive(c *gin.Context) {
	logID, ok := parseContentModerationLogID(c)
	if !ok {
		return
	}
	deleted, err := h.service.DeleteArchive(c.Request.Context(), logID, contentModerationActorID(c), contentModerationAdminRequestID(c))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": deleted})
}

func parseContentModerationLogID(c *gin.Context) (int64, bool) {
	logID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || logID <= 0 {
		response.BadRequest(c, "Invalid log id")
		return 0, false
	}
	return logID, true
}

func contentModerationActorID(c *gin.Context) int64 {
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		return 0
	}
	return subject.UserID
}

func contentModerationAdminRequestID(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	requestID, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
	return strings.TrimSpace(requestID)
}

func parseContentModerationDate(raw string) (time.Time, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, false, nil
	}
	t, err := time.Parse("2006-01-02", raw)
	return t, err == nil, err
}
