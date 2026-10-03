package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 局部响应结构体：经蛇形 JSON 键解码，校验响应字段名与 API 契约一致
type tokenQuotaRow struct {
	TokenID   int    `json:"token_id"`
	TokenName string `json:"token_name"`
	CreatedAt int64  `json:"created_at"`
	Count     int    `json:"count"`
	Quota     int    `json:"quota"`
	TokenUsed int    `json:"token_used"`
}

type tokenQuotaResponse struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Data    []tokenQuotaRow `json:"data"`
}

func setupTokenQuotaControllerTestDB(t *testing.T) {
	t.Helper()
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Token{}, &model.QuotaData{}))
}

func seedTokenQuotaData(t *testing.T, rows ...*model.QuotaData) {
	t.Helper()
	for _, row := range rows {
		require.NoError(t, model.DB.Create(row).Error)
	}
}

func runGetQuotaDatesByToken(t *testing.T, query string) tokenQuotaResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/data/tokens?"+query, nil)

	GetQuotaDatesByToken(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload tokenQuotaResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	return payload
}

func TestGetQuotaDatesByTokenReturnsAggregatedRows(t *testing.T) {
	setupTokenQuotaControllerTestDB(t)
	require.NoError(t, model.DB.Create(&model.Token{Id: 11, UserId: 1, Key: "sk-primary", Name: "primary"}).Error)
	seedTokenQuotaData(t,
		&model.QuotaData{TokenID: 11, CreatedAt: 1000, Count: 2, Quota: 100, TokenUsed: 40},
		&model.QuotaData{TokenID: 0, CreatedAt: 1000, Count: 1, Quota: 30, TokenUsed: 10},
	)

	payload := runGetQuotaDatesByToken(t, "start_timestamp=1000&end_timestamp=2000")

	require.True(t, payload.Success, payload.Message)
	require.Len(t, payload.Data, 2)
	byTokenID := make(map[int]tokenQuotaRow, len(payload.Data))
	for _, row := range payload.Data {
		byTokenID[row.TokenID] = row
	}
	require.Contains(t, byTokenID, 11)
	require.Contains(t, byTokenID, 0)
	require.Equal(t, "primary", byTokenID[11].TokenName)
	require.Equal(t, 100, byTokenID[11].Quota)
	require.Equal(t, "", byTokenID[0].TokenName)
	require.Equal(t, 30, byTokenID[0].Quota)
}

func TestGetQuotaDatesByTokenRejectsInvalidTimeParams(t *testing.T) {
	setupTokenQuotaControllerTestDB(t)

	cases := []struct {
		name    string
		query   string
		message string
	}{
		{"missing start", "end_timestamp=2000", "invalid start_timestamp"},
		{"non-integer start", "start_timestamp=bad&end_timestamp=2000", "invalid start_timestamp"},
		{"zero end", "start_timestamp=1000&end_timestamp=0", "invalid end_timestamp"},
		{"end before start", "start_timestamp=2000&end_timestamp=1000", "invalid time range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := runGetQuotaDatesByToken(t, tc.query)
			require.False(t, payload.Success)
			require.Equal(t, tc.message, payload.Message)
		})
	}
}

func TestGetQuotaDatesByTokenRejectsOversizedRange(t *testing.T) {
	setupTokenQuotaControllerTestDB(t)

	payload := runGetQuotaDatesByToken(t, "start_timestamp=1000&end_timestamp=2593001")

	require.False(t, payload.Success)
	require.Equal(t, "时间跨度不能超过 1 个月", payload.Message)
}

func TestGetQuotaDatesByTokenAcceptsMaxSpan(t *testing.T) {
	setupTokenQuotaControllerTestDB(t)
	seedTokenQuotaData(t, &model.QuotaData{TokenID: 11, CreatedAt: 1500, Count: 1, Quota: 50, TokenUsed: 20})

	payload := runGetQuotaDatesByToken(t, "start_timestamp=1000&end_timestamp=2593000")

	require.True(t, payload.Success, payload.Message)
	require.Len(t, payload.Data, 1)
	require.Equal(t, 11, payload.Data[0].TokenID)
}

func TestGetQuotaDatesByTokenFailsOnDbError(t *testing.T) {
	setupTokenQuotaControllerTestDB(t)
	require.NoError(t, model.DB.Migrator().DropTable("quota_data"))

	payload := runGetQuotaDatesByToken(t, "start_timestamp=1000&end_timestamp=2000")

	require.False(t, payload.Success)
	require.NotEmpty(t, payload.Message)
}
