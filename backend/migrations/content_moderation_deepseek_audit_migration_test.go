package migrations

import (
	"os"
	"strings"
	"testing"
)

func TestContentModerationDeepSeekAuditMigrationIsForwardCompatible(t *testing.T) {
	raw, err := os.ReadFile("225_content_moderation_deepseek_audit.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	for _, column := range []string{
		"deepseek_confidence", "deepseek_category", "deepseek_reason",
		"review_outcome", "reviewer_disagreement", "review_attempts",
	} {
		if !strings.Contains(sql, "ADD COLUMN IF NOT EXISTS "+column) {
			t.Fatalf("migration must add %s idempotently", column)
		}
	}
	if !strings.Contains(sql, "jsonb_typeof(review_attempts) = 'array'") {
		t.Fatal("review_attempts must be constrained to a JSON array")
	}
	if !strings.Contains(sql, "ADD COLUMN IF NOT EXISTS deepseek_confidence DECIMAL(6, 5),") {
		t.Fatal("historical rows must receive a nullable DeepSeek confidence")
	}
	if strings.Contains(sql, "deepseek_confidence DECIMAL(6, 5) NOT NULL") ||
		strings.Contains(sql, "deepseek_confidence DECIMAL(6, 5) DEFAULT") {
		t.Fatal("DeepSeek confidence must not synthesize a zero value for historical rows")
	}
	for _, repair := range []string{
		"ALTER COLUMN deepseek_confidence DROP DEFAULT",
		"ALTER COLUMN deepseek_confidence DROP NOT NULL",
		"SET deepseek_confidence = NULL",
	} {
		if !strings.Contains(sql, repair) {
			t.Fatalf("migration must repair pre-release schemas with %q", repair)
		}
	}
	for _, forbidden := range []string{"authorization", "api_key", "ciphertext", "provider_response"} {
		if strings.Contains(strings.ToLower(sql), forbidden) {
			t.Fatalf("audit migration must not persist %s", forbidden)
		}
	}
}
