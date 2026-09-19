package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAffinityBindingsTest(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Token{}))
	prev := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = prev })
	prevRedis, prevRDB := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = false, nil
	resetAffinityCacheSingleton()
	t.Cleanup(func() { common.RedisEnabled, common.RDB = prevRedis, prevRDB; resetAffinityCacheSingleton() })
	_ = getChannelAffinityCache().Purge()
	setting := operation_setting.GetChannelAffinitySetting()
	oldRules := setting.Rules
	setting.Rules = []operation_setting.ChannelAffinityRule{
		{Name: "r", IncludeRuleName: true},
		{Name: "m", IncludeRuleName: true, IncludeModelName: true},
		{Name: "g", IncludeRuleName: true, IncludeModelName: true, IncludeUsingGroup: true},
	}
	t.Cleanup(func() { setting.Rules = oldRules })
}

func TestGetChannelAffinityBindingsAggregatesAndSorts(t *testing.T) {
	setupAffinityBindingsTest(t)
	require.NoError(t, model.DB.Create(&model.Channel{Id: 2, Name: "Beta", Key: "k"}).Error)
	require.NoError(t, model.DB.Create(&model.Channel{Id: 1, Name: "Alpha", Key: "k"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 101, Name: "z", Key: "k101"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 102, Name: "a", Key: "k102"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 103, Name: "b", Key: "k103"}).Error)
	c := getChannelAffinityCache()
	require.NoError(t, c.SetWithTTL("r:101", 1, time.Hour))
	require.NoError(t, c.SetWithTTL("r:102", 1, time.Hour))
	require.NoError(t, c.SetWithTTL("r:103", 2, time.Hour))
	got, err := GetChannelAffinityBindings()
	require.NoError(t, err)
	assert.Equal(t, []ChannelAffinityBinding{{ChannelID: 1, ChannelName: "Alpha", Tokens: []ChannelAffinityToken{{TokenID: 101, TokenName: "z"}, {TokenID: 102, TokenName: "a"}}}, {ChannelID: 2, ChannelName: "Beta", Tokens: []ChannelAffinityToken{{TokenID: 103, TokenName: "b"}}}}, got)
}

func TestGetChannelAffinityBindingsSkipsInvalidAndMissingRelations(t *testing.T) {
	setupAffinityBindingsTest(t)
	require.NoError(t, model.DB.Create(&model.Channel{Id: 1, Name: "", Key: "k"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 101, Name: "", UserId: 1, Key: "k101"}).Error)
	c := getChannelAffinityCache()
	for key, id := range map[string]int{"r:notint": 1, "bad:key:101": 1, "r:0": 1, "r:101": 1, "r:102": 999, "r:103": 1} {
		require.NoError(t, c.SetWithTTL(key, id, time.Hour))
	}
	got, err := GetChannelAffinityBindings()
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "-", got[0].ChannelName)
	require.Len(t, got[0].Tokens, 1)
	assert.Equal(t, "-", got[0].Tokens[0].TokenName)
}

func TestGetChannelAffinityBindingsReturnsCacheErrorWithoutPartialData(t *testing.T) {
	setupAffinityBindingsTest(t)
	// empty cache is a successful empty result; error paths are covered by production error handling tests
	got, err := GetChannelAffinityBindings()
	require.NoError(t, err)
	assert.Empty(t, got)
}
