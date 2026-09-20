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
	t.Cleanup(func() {
		model.DB = prev
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})
	prevRedis, prevRDB := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = false, nil
	resetAffinityCacheSingleton()
	t.Cleanup(func() { common.RedisEnabled, common.RDB = prevRedis, prevRDB; resetAffinityCacheSingleton() })
	_ = getChannelAffinityCache().Purge()
	setting := operation_setting.GetChannelAffinitySetting()
	oldRules := setting.Rules
	setting.Rules = []operation_setting.ChannelAffinityRule{
		{Name: "r", IncludeRuleName: true, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}},
		{Name: "m", IncludeRuleName: true, IncludeModelName: true, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}},
		{Name: "g", IncludeRuleName: true, IncludeModelName: true, IncludeUsingGroup: true, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}},
	}
	t.Cleanup(func() { setting.Rules = oldRules })
}

func TestGetChannelAffinityBindingsIgnoresNonTokenIDKeySources(t *testing.T) {
	setupAffinityBindingsTest(t)
	setting := operation_setting.GetChannelAffinitySetting()
	setting.Rules = append(setting.Rules,
		operation_setting.ChannelAffinityRule{Name: "header", IncludeRuleName: true, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Affinity-Key"}}},
		operation_setting.ChannelAffinityRule{Name: "gjson", IncludeRuleName: true, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}}},
	)
	require.NoError(t, model.DB.Create(&model.Channel{Id: 1, Name: "Alpha", Key: "k"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 101, Name: "tok", Key: "k101"}).Error)
	c := getChannelAffinityCache()
	require.NoError(t, c.SetWithTTL("header:101", 1, time.Hour))
	require.NoError(t, c.SetWithTTL("gjson:101", 1, time.Hour))
	require.NoError(t, c.SetWithTTL("r:101", 1, time.Hour))

	got, err := GetChannelAffinityBindings()
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 101, got[0].Tokens[0].TokenID)
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

func TestGetChannelAffinityBindingsAcceptsTokenOnlyKey(t *testing.T) {
	setupAffinityBindingsTest(t)
	setting := operation_setting.GetChannelAffinitySetting()
	setting.Rules = []operation_setting.ChannelAffinityRule{{
		KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}},
	}}
	require.NoError(t, model.DB.Create(&model.Channel{Id: 1, Name: "Alpha", Key: "k"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 101, Name: "tok", Key: "k101"}).Error)
	require.NoError(t, getChannelAffinityCache().SetWithTTL("101", 1, time.Hour))

	got, err := GetChannelAffinityBindings()
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 101, got[0].Tokens[0].TokenID)
}

func TestGetChannelAffinityBindingsAcceptsColonInRuleName(t *testing.T) {
	setupAffinityBindingsTest(t)
	setting := operation_setting.GetChannelAffinitySetting()
	setting.Rules = []operation_setting.ChannelAffinityRule{{
		Name:            "rule:with:colon",
		IncludeRuleName: true,
		KeySources:      []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}},
	}}
	require.NoError(t, model.DB.Create(&model.Channel{Id: 1, Name: "Alpha", Key: "k"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 101, Name: "tok", Key: "k101"}).Error)
	require.NoError(t, getChannelAffinityCache().SetWithTTL("rule:with:colon:101", 1, time.Hour))

	got, err := GetChannelAffinityBindings()
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, 101, got[0].Tokens[0].TokenID)
}

func TestGetChannelAffinityBindingsRequiresActualTokenIDSource(t *testing.T) {
	setupAffinityBindingsTest(t)
	setting := operation_setting.GetChannelAffinitySetting()
	setting.Rules = []operation_setting.ChannelAffinityRule{{
		Name:            "mixed",
		IncludeRuleName: true,
		KeySources: []operation_setting.ChannelAffinityKeySource{
			{Type: "request_header", Key: "X-Affinity-Key"},
			{Type: "context_int", Key: "token_id"},
		},
	}}
	require.NoError(t, model.DB.Create(&model.Channel{Id: 1, Name: "Alpha", Key: "k"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 101, Name: "header", Key: "k101"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 202, Name: "context", Key: "k202"}).Error)
	require.NoError(t, getChannelAffinityCache().SetWithTTL("mixed:101", 1, time.Hour))

	got, err := GetChannelAffinityBindings()
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGetChannelAffinityBindingsReturnsCacheErrorWithoutPartialData(t *testing.T) {
	setupAffinityBindingsTest(t)
	got, err := GetChannelAffinityBindings()
	require.NoError(t, err)
	assert.Empty(t, got)

	require.NoError(t, model.DB.Create(&model.Channel{Id: 1, Name: "Alpha", Key: "k"}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 101, Name: "tok", Key: "k101"}).Error)
	require.NoError(t, getChannelAffinityCache().SetWithTTL("r:101", 1, time.Hour))
	sqlDB, dbErr := model.DB.DB()
	require.NoError(t, dbErr)
	require.NoError(t, sqlDB.Close())

	got, err = GetChannelAffinityBindings()
	require.Error(t, err)
	assert.Nil(t, got)
}
