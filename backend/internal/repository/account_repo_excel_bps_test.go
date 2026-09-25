package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBulkUpdateExcelBPSExtra(t *testing.T) {
	tests := []struct {
		name    string
		extra   map[string]any
		removed []string
	}{
		{
			name:    "all models removes scope key",
			extra:   map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": nil, "openai_excel_bps_cache_creation_as_input": false},
			removed: []string{"openai_excel_bps_models", "openai_excel_bps_cache_creation_as_input"},
		},
		{
			name:  "empty scope remains explicit",
			extra: map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": []string{}, "openai_excel_bps_cache_creation_as_input": true},
		},
		{
			name:  "selected scope remains explicit",
			extra: map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": []string{"gpt-6-astra"}},
		},
		{
			name:    "disabled removes all BPS settings",
			extra:   map[string]any{"openai_excel_bps": false},
			removed: []string{"openai_excel_bps", "openai_excel_bps_models", "openai_excel_bps_cache_creation_as_input", "openai_excel_bps_auto_disable_on_403"},
		},
		{
			name:  "unrelated changes preserve BPS settings",
			extra: map[string]any{"openai_passthrough": true},
		},
		{
			name:    "auto disable false removes opt-in",
			extra:   map[string]any{"openai_excel_bps_auto_disable_on_403": false},
			removed: []string{"openai_excel_bps_auto_disable_on_403"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &recordingSQLExecutor{result: rowsAffectedResult(0)}
			repo := newAccountRepositoryWithSQL(nil, exec, nil)
			_, err := repo.BulkUpdate(context.Background(), []int64{27, 28}, service.AccountBulkUpdate{Extra: tt.extra})
			require.NoError(t, err)
			require.Len(t, exec.execQueries, 1)
			query := normalizeSQLWhitespace(exec.execQueries[0])
			expression := "COALESCE(extra, '{}'::jsonb) || $1::jsonb"
			if tt.name == "disabled removes all BPS settings" {
				expression = "(" + expression + ") - 'openai_excel_bps' - 'openai_excel_bps_models' - 'openai_excel_bps_cache_creation_as_input' - 'openai_excel_bps_auto_disable_on_403'"
			} else {
				for _, key := range tt.removed {
					expression = "(" + expression + ") - '" + key + "'"
				}
			}
			require.Equal(t, "UPDATE accounts SET extra = "+expression+", updated_at = NOW() WHERE id = ANY($2) AND deleted_at IS NULL", query)
			payload, ok := exec.execArgs[0][0].([]byte)
			require.True(t, ok)
			want, err := json.Marshal(tt.extra)
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(payload))
		})
	}
}
