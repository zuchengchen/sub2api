package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// These requests characterize the retired HTTP contract, including validation
// that must run before the deprecation response and validation it must bypass.
func TestDataManagementHandler_RequestCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const socketPath = "/unused/data-management.sock"
	h := NewDataManagementHandler(service.NewDataManagementServiceWithOptions(socketPath, 0))
	const badRequest = http.StatusBadRequest
	const deprecated = http.StatusServiceUnavailable
	tests := []struct {
		name    string
		handler gin.HandlerFunc
		params  gin.Params
		body    string
		query   string
		status  int
		message string
	}{
		{name: "config malformed JSON", handler: h.UpdateConfig, body: "{", status: badRequest, message: "Invalid request:"},
		{name: "config array", handler: h.UpdateConfig, body: "[]", status: badRequest, message: "type service.DataManagementConfig"},
		{name: "config nested postgres type", handler: h.UpdateConfig, body: `{"postgres":{"port":"invalid"}}`, status: badRequest, message: "postgres.port"},
		{name: "config nested redis type", handler: h.UpdateConfig, body: `{"redis":{"db":false}}`, status: badRequest, message: "redis.db"},
		{name: "config nested s3 type", handler: h.UpdateConfig, body: `{"s3":{"enabled":"yes"}}`, status: badRequest, message: "s3.enabled"},
		{name: "config null", handler: h.UpdateConfig, body: "null", status: deprecated},
		{name: "config valid", handler: h.UpdateConfig, body: `{"retention_days":7}`, status: deprecated},
		{name: "test s3 missing region", handler: h.TestS3, body: `{"bucket":"test"}`, status: badRequest, message: "TestS3ConnectionRequest.Region"},
		{name: "test s3 missing bucket", handler: h.TestS3, body: `{"region":"test"}`, status: badRequest, message: "TestS3ConnectionRequest.Bucket"},
		{name: "test s3 valid", handler: h.TestS3, body: `{"region":"test","bucket":"test"}`, status: deprecated},
		{name: "backup missing type", handler: h.CreateBackupJob, body: `{}`, status: badRequest, message: "CreateBackupJobRequest.BackupType"},
		{name: "backup unsupported type", handler: h.CreateBackupJob, body: `{"backup_type":"sqlite"}`, status: badRequest, message: "oneof"},
		{name: "backup invalid boolean", handler: h.CreateBackupJob, body: `{"backup_type":"full","upload_to_s3":"yes"}`, status: badRequest, message: "upload_to_s3"},
		{name: "backup valid", handler: h.CreateBackupJob, body: `{"backup_type":"full","idempotency_key":"test"}`, status: deprecated},
		{name: "list sources missing type", handler: h.ListSourceProfiles, status: badRequest, message: "Invalid source_type"},
		{name: "list sources unsupported type", handler: h.ListSourceProfiles, params: gin.Params{{Key: "source_type", Value: "sqlite"}}, status: badRequest, message: "source_type must be postgres or redis"},
		{name: "list sources trimmed type", handler: h.ListSourceProfiles, params: gin.Params{{Key: "source_type", Value: " postgres "}}, status: deprecated},
		{name: "create source type before JSON", handler: h.CreateSourceProfile, body: "{", status: badRequest, message: "source_type must be postgres or redis"},
		{name: "create source missing profile", handler: h.CreateSourceProfile, params: gin.Params{{Key: "source_type", Value: "postgres"}}, body: `{"name":"test","config":{}}`, status: badRequest, message: "CreateSourceProfileRequest.ProfileID"},
		{name: "create source missing name", handler: h.CreateSourceProfile, params: gin.Params{{Key: "source_type", Value: "postgres"}}, body: `{"profile_id":"test","config":{}}`, status: badRequest, message: "CreateSourceProfileRequest.Name"},
		{name: "create source invalid config", handler: h.CreateSourceProfile, params: gin.Params{{Key: "source_type", Value: "postgres"}}, body: `{"profile_id":"test","name":"test","config":{"port":"invalid"}}`, status: badRequest, message: "config.port"},
		{name: "create source valid", handler: h.CreateSourceProfile, params: gin.Params{{Key: "source_type", Value: "redis"}}, body: `{"profile_id":"test","name":"test","config":{"addr":"localhost:6379"}}`, status: deprecated},
		{name: "update source type before JSON", handler: h.UpdateSourceProfile, body: "{", status: badRequest, message: "source_type must be postgres or redis"},
		{name: "update source profile before JSON", handler: h.UpdateSourceProfile, params: gin.Params{{Key: "source_type", Value: "postgres"}}, body: "{", status: badRequest, message: "Invalid profile_id"},
		{name: "update source missing name", handler: h.UpdateSourceProfile, params: gin.Params{{Key: "source_type", Value: "postgres"}, {Key: "profile_id", Value: "test"}}, body: `{"config":{}}`, status: badRequest, message: "UpdateSourceProfileRequest.Name"},
		{name: "update source valid", handler: h.UpdateSourceProfile, params: gin.Params{{Key: "source_type", Value: "postgres"}, {Key: "profile_id", Value: "test"}}, body: `{"name":"test","config":{"host":"localhost"}}`, status: deprecated},
		{name: "delete source unsupported type", handler: h.DeleteSourceProfile, params: gin.Params{{Key: "source_type", Value: "sqlite"}}, status: badRequest, message: "source_type must be postgres or redis"},
		{name: "delete source missing profile", handler: h.DeleteSourceProfile, params: gin.Params{{Key: "source_type", Value: "redis"}}, status: badRequest, message: "Invalid profile_id"},
		{name: "delete source valid", handler: h.DeleteSourceProfile, params: gin.Params{{Key: "source_type", Value: "redis"}, {Key: "profile_id", Value: "test"}}, status: deprecated},
		{name: "activate source unsupported type", handler: h.SetActiveSourceProfile, params: gin.Params{{Key: "source_type", Value: "sqlite"}}, status: badRequest, message: "source_type must be postgres or redis"},
		{name: "activate source missing profile", handler: h.SetActiveSourceProfile, params: gin.Params{{Key: "source_type", Value: "postgres"}, {Key: "profile_id", Value: " "}}, status: badRequest, message: "Invalid profile_id"},
		{name: "activate source valid", handler: h.SetActiveSourceProfile, params: gin.Params{{Key: "source_type", Value: "postgres"}, {Key: "profile_id", Value: "test"}}, status: deprecated},
		{name: "create s3 missing profile", handler: h.CreateS3Profile, body: `{"name":"test"}`, status: badRequest, message: "CreateS3ProfileRequest.ProfileID"},
		{name: "create s3 missing name", handler: h.CreateS3Profile, body: `{"profile_id":"test"}`, status: badRequest, message: "CreateS3ProfileRequest.Name"},
		{name: "create s3 invalid boolean", handler: h.CreateS3Profile, body: `{"profile_id":"test","name":"test","enabled":"yes"}`, status: badRequest, message: "enabled"},
		{name: "create s3 valid", handler: h.CreateS3Profile, body: `{"profile_id":"test","name":"test"}`, status: deprecated},
		{name: "update s3 JSON before profile", handler: h.UpdateS3Profile, body: "{", status: badRequest, message: "Invalid request:"},
		{name: "update s3 name before profile", handler: h.UpdateS3Profile, body: `{}`, status: badRequest, message: "UpdateS3ProfileRequest.Name"},
		{name: "update s3 missing profile", handler: h.UpdateS3Profile, body: `{"name":"test"}`, status: badRequest, message: "Invalid profile_id"},
		{name: "update s3 valid", handler: h.UpdateS3Profile, params: gin.Params{{Key: "profile_id", Value: "test"}}, body: `{"name":"test"}`, status: deprecated},
		{name: "delete s3 missing profile", handler: h.DeleteS3Profile, status: badRequest, message: "Invalid profile_id"},
		{name: "delete s3 valid", handler: h.DeleteS3Profile, params: gin.Params{{Key: "profile_id", Value: "test"}}, status: deprecated},
		{name: "activate s3 missing profile", handler: h.SetActiveS3Profile, status: badRequest, message: "Invalid profile_id"},
		{name: "activate s3 valid", handler: h.SetActiveS3Profile, params: gin.Params{{Key: "profile_id", Value: "test"}}, status: deprecated},
		{name: "list backups before invalid page size", handler: h.ListBackupJobs, query: "?page_size=invalid", status: deprecated},
		{name: "list backups before zero page size", handler: h.ListBackupJobs, query: "?page_size=0", status: deprecated},
		{name: "get backup missing job", handler: h.GetBackupJob, status: badRequest, message: "Invalid backup job ID"},
		{name: "get backup valid", handler: h.GetBackupJob, params: gin.Params{{Key: "job_id", Value: "test"}}, status: deprecated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Params = tt.params
			ctx.Request = httptest.NewRequest(http.MethodPost, "/"+tt.query, strings.NewReader(tt.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			tt.handler(ctx)

			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			var envelope struct {
				Code     int               `json:"code"`
				Message  string            `json:"message"`
				Reason   string            `json:"reason"`
				Metadata map[string]string `json:"metadata"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
			require.Equal(t, tt.status, envelope.Code)
			if tt.status == deprecated {
				require.Equal(t, "data management feature is deprecated", envelope.Message)
				require.Equal(t, service.DataManagementDeprecatedReason, envelope.Reason)
				require.Equal(t, map[string]string{"socket_path": socketPath}, envelope.Metadata)
			} else {
				require.Contains(t, envelope.Message, tt.message)
				require.Empty(t, envelope.Reason)
				require.Empty(t, envelope.Metadata)
			}
		})
	}
}
