package gateway

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"agent-gateway/internal/audit"
	"agent-gateway/internal/config"
	"agent-gateway/internal/telemetry"
	"agent-gateway/internal/upstream"
)

func TestUnauthorizedMCPDoesNotRecordBearerToken(t *testing.T) {
	sr, cleanup := telemetry.SetupTest()
	defer cleanup()
	log, err := audit.New(filepath.Join(t.TempDir(), "audit.jsonl"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	h := NewHandler(&config.Config{APIKeys: []config.APIKey{{Name: "reviewer", Key: "real-key", Allow: []string{"*"}}}}, upstream.NewManager(nil), log)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_GW")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	dump := telemetry.DumpAttrs(sr)
	require.NotContains(t, dump, "ghp_SYNTHETICTESTTOKEN_DO_NOT_USE_GW")
	require.NotContains(t, dump, "real-key")
	require.Equal(t, telemetry.ClassGatewayAuth, telemetry.AttrsOf(sr, telemetry.SpanExecuteTool)[telemetry.AttrErrorClass])
}
