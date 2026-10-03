package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetTokenQuotaDataTables(t *testing.T) {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM quota_data").Error)
	require.NoError(t, DB.Exec("DELETE FROM tokens").Error)
}

func seedTokenQuotaData(t *testing.T, quotaData QuotaData) {
	t.Helper()
	require.NoError(t, DB.Create(&quotaData).Error)
}

func findTokenQuotaDataRow(t *testing.T, rows []*TokenQuotaData, tokenID int, createdAt int64) *TokenQuotaData {
	t.Helper()
	for _, row := range rows {
		if row.TokenID == tokenID && row.CreatedAt == createdAt {
			return row
		}
	}
	t.Fatalf("row (token_id=%d, created_at=%d) not found", tokenID, createdAt)
	return nil
}

func TestGetQuotaDataGroupByTokenAggregatesByTokenAndHour(t *testing.T) {
	resetTokenQuotaDataTables(t)

	seedTokenQuotaData(t, QuotaData{TokenID: 11, CreatedAt: 1000, Count: 2, Quota: 100, TokenUsed: 40})
	seedTokenQuotaData(t, QuotaData{TokenID: 11, CreatedAt: 1100, Count: 1, Quota: 50, TokenUsed: 20})
	seedTokenQuotaData(t, QuotaData{TokenID: 22, CreatedAt: 1200, Count: 3, Quota: 70, TokenUsed: 30})

	rows, err := GetQuotaDataGroupByToken(900, 2000)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	rowAt1000 := findTokenQuotaDataRow(t, rows, 11, 1000)
	assert.Equal(t, 2, rowAt1000.Count)
	assert.Equal(t, 100, rowAt1000.Quota)
	assert.Equal(t, 40, rowAt1000.TokenUsed)

	rowAt1100 := findTokenQuotaDataRow(t, rows, 11, 1100)
	assert.Equal(t, 1, rowAt1100.Count)
	assert.Equal(t, 50, rowAt1100.Quota)
	assert.Equal(t, 20, rowAt1100.TokenUsed)

	token11Rows := 0
	for _, row := range rows {
		if row.TokenID == 11 {
			token11Rows++
		}
	}
	assert.Equal(t, 2, token11Rows)
}

func TestGetQuotaDataGroupByTokenAggregatesSameHourRows(t *testing.T) {
	resetTokenQuotaDataTables(t)

	seedTokenQuotaData(t, QuotaData{TokenID: 11, CreatedAt: 1000, Count: 2, Quota: 100, TokenUsed: 40})
	seedTokenQuotaData(t, QuotaData{TokenID: 11, CreatedAt: 1000, Count: 1, Quota: 50, TokenUsed: 20})

	rows, err := GetQuotaDataGroupByToken(900, 2000)
	require.NoError(t, err)
	require.Len(t, rows, 1)

	row := findTokenQuotaDataRow(t, rows, 11, 1000)
	assert.Equal(t, 3, row.Count)
	assert.Equal(t, 150, row.Quota)
	assert.Equal(t, 60, row.TokenUsed)
}

func TestGetQuotaDataGroupByTokenFillsTokenNameAndFallsBack(t *testing.T) {
	resetTokenQuotaDataTables(t)

	require.NoError(t, DB.Create(&Token{Id: 11, UserId: 1, Key: "sk-tok-primary", Name: "primary"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 22, UserId: 1, Key: "sk-tok-backup", Name: "backup"}).Error)
	require.NoError(t, DB.Create(&Token{Id: 33, UserId: 1, Key: "sk-tok-empty", Name: ""}).Error)
	require.NoError(t, DB.Delete(&Token{Id: 22}).Error)

	seedTokenQuotaData(t, QuotaData{TokenID: 11, CreatedAt: 1000, Count: 1, Quota: 110, TokenUsed: 10})
	seedTokenQuotaData(t, QuotaData{TokenID: 22, CreatedAt: 1000, Count: 1, Quota: 220, TokenUsed: 20})
	seedTokenQuotaData(t, QuotaData{TokenID: 33, CreatedAt: 1000, Count: 1, Quota: 330, TokenUsed: 30})
	seedTokenQuotaData(t, QuotaData{TokenID: 0, CreatedAt: 1000, Count: 1, Quota: 40, TokenUsed: 40})

	rows, err := GetQuotaDataGroupByToken(900, 2000)
	require.NoError(t, err)
	require.Len(t, rows, 4)

	assert.Equal(t, "primary", findTokenQuotaDataRow(t, rows, 11, 1000).TokenName)
	// 软删除令牌回退为空串，由前端显示 #token_id
	assert.Equal(t, "", findTokenQuotaDataRow(t, rows, 22, 1000).TokenName)
	// 空名称回退为空串
	assert.Equal(t, "", findTokenQuotaDataRow(t, rows, 33, 1000).TokenName)
	// token_id 为 0：名称恒空，聚合数值照常计入
	zeroRow := findTokenQuotaDataRow(t, rows, 0, 1000)
	assert.Equal(t, "", zeroRow.TokenName)
	assert.Equal(t, 40, zeroRow.Quota)
}

func TestGetQuotaDataGroupByTokenFiltersByTimeWindow(t *testing.T) {
	resetTokenQuotaDataTables(t)

	seedTokenQuotaData(t, QuotaData{TokenID: 11, CreatedAt: 999, Count: 1, Quota: 10, TokenUsed: 1})
	seedTokenQuotaData(t, QuotaData{TokenID: 11, CreatedAt: 1000, Count: 1, Quota: 20, TokenUsed: 2})
	seedTokenQuotaData(t, QuotaData{TokenID: 11, CreatedAt: 2001, Count: 1, Quota: 30, TokenUsed: 3})

	rows, err := GetQuotaDataGroupByToken(1000, 2000)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := findTokenQuotaDataRow(t, rows, 11, 1000)
	assert.Equal(t, 20, row.Quota)
}

func TestGetQuotaDataGroupByTokenReturnsEmptyWhenNoData(t *testing.T) {
	resetTokenQuotaDataTables(t)

	rows, err := GetQuotaDataGroupByToken(1000, 2000)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestGetQuotaDataGroupByTokenSkipsNameLookupForZeroOnlyAndIncludesEndTime(t *testing.T) {
	resetTokenQuotaDataTables(t)

	seedTokenQuotaData(t, QuotaData{TokenID: 0, CreatedAt: 2000, Count: 1, Quota: 40, TokenUsed: 40})
	seedTokenQuotaData(t, QuotaData{TokenID: 0, CreatedAt: 2001, Count: 1, Quota: 50, TokenUsed: 50})

	rows, err := GetQuotaDataGroupByToken(1000, 2000)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := findTokenQuotaDataRow(t, rows, 0, 2000)
	assert.Equal(t, "", row.TokenName)
	assert.Equal(t, 40, row.Quota)
}
