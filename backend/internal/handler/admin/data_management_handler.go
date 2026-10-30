package admin

import (
	"context"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// DataManagementHandler preserves the retired API routes and their request
// validation. Operations only report unavailability; they never contact an agent.
type DataManagementHandler struct {
	dataManagementService dataManagementService
}

func NewDataManagementHandler(dataManagementService *service.DataManagementService) *DataManagementHandler {
	return &DataManagementHandler{dataManagementService: dataManagementService}
}

type dataManagementService interface {
	EnsureAgentEnabled(ctx context.Context) error
	GetAgentHealth(ctx context.Context) service.DataManagementAgentHealth
}

type TestS3ConnectionRequest struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region" binding:"required"`
	Bucket          string `json:"bucket" binding:"required"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"`
	ForcePathStyle  bool   `json:"force_path_style"`
	UseSSL          bool   `json:"use_ssl"`
}

type CreateBackupJobRequest struct {
	BackupType     string `json:"backup_type" binding:"required,oneof=postgres redis full"`
	UploadToS3     bool   `json:"upload_to_s3"`
	S3ProfileID    string `json:"s3_profile_id"`
	PostgresID     string `json:"postgres_profile_id"`
	RedisID        string `json:"redis_profile_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type CreateSourceProfileRequest struct {
	ProfileID string                             `json:"profile_id" binding:"required"`
	Name      string                             `json:"name" binding:"required"`
	Config    service.DataManagementSourceConfig `json:"config" binding:"required"`
	SetActive bool                               `json:"set_active"`
}

type UpdateSourceProfileRequest struct {
	Name   string                             `json:"name" binding:"required"`
	Config service.DataManagementSourceConfig `json:"config" binding:"required"`
}

type CreateS3ProfileRequest struct {
	ProfileID       string `json:"profile_id" binding:"required"`
	Name            string `json:"name" binding:"required"`
	Enabled         bool   `json:"enabled"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"`
	ForcePathStyle  bool   `json:"force_path_style"`
	UseSSL          bool   `json:"use_ssl"`
	SetActive       bool   `json:"set_active"`
}

type UpdateS3ProfileRequest struct {
	Name            string `json:"name" binding:"required"`
	Enabled         bool   `json:"enabled"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"`
	ForcePathStyle  bool   `json:"force_path_style"`
	UseSSL          bool   `json:"use_ssl"`
}

func (h *DataManagementHandler) GetAgentHealth(c *gin.Context) {
	health := h.getAgentHealth(c)
	payload := gin.H{
		"enabled":     health.Enabled,
		"reason":      health.Reason,
		"socket_path": health.SocketPath,
	}
	if health.Agent != nil {
		payload["agent"] = gin.H{
			"status":         health.Agent.Status,
			"version":        health.Agent.Version,
			"uptime_seconds": health.Agent.UptimeSeconds,
		}
	}
	response.Success(c, payload)
}

func (h *DataManagementHandler) GetConfig(c *gin.Context) {
	h.respondUnavailable(c)
}

func (h *DataManagementHandler) UpdateConfig(c *gin.Context) {
	var req service.DataManagementConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) TestS3(c *gin.Context) {
	var req TestS3ConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) CreateBackupJob(c *gin.Context) {
	var req CreateBackupJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) ListSourceProfiles(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType == "" {
		response.BadRequest(c, "Invalid source_type")
		return
	}
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) CreateSourceProfile(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}

	var req CreateSourceProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) UpdateSourceProfile(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	var req UpdateSourceProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) DeleteSourceProfile(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) SetActiveSourceProfile(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) ListS3Profiles(c *gin.Context) {
	h.respondUnavailable(c)
}

func (h *DataManagementHandler) CreateS3Profile(c *gin.Context) {
	var req CreateS3ProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) UpdateS3Profile(c *gin.Context) {
	var req UpdateS3ProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) DeleteS3Profile(c *gin.Context) {
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) SetActiveS3Profile(c *gin.Context) {
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) ListBackupJobs(c *gin.Context) {
	// Deprecation historically takes precedence over page_size validation.
	h.respondUnavailable(c)
}

func (h *DataManagementHandler) GetBackupJob(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("job_id"))
	if jobID == "" {
		response.BadRequest(c, "Invalid backup job ID")
		return
	}

	h.respondUnavailable(c)
}

func (h *DataManagementHandler) respondUnavailable(c *gin.Context) {
	if h.dataManagementService == nil {
		err := infraerrors.ServiceUnavailable(
			service.DataManagementAgentUnavailableReason,
			"data management agent service is not configured",
		).WithMetadata(map[string]string{"socket_path": service.DefaultDataManagementAgentSocketPath})
		response.ErrorFrom(c, err)
		return
	}

	response.ErrorFrom(c, h.dataManagementService.EnsureAgentEnabled(c.Request.Context()))
}

func (h *DataManagementHandler) getAgentHealth(c *gin.Context) service.DataManagementAgentHealth {
	if h.dataManagementService == nil {
		return service.DataManagementAgentHealth{
			Enabled:    false,
			Reason:     service.DataManagementAgentUnavailableReason,
			SocketPath: service.DefaultDataManagementAgentSocketPath,
		}
	}
	return h.dataManagementService.GetAgentHealth(c.Request.Context())
}
