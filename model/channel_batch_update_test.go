package model

import (
	"errors"
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func ptrString(v string) *string { return &v }
func ptrUint(v uint) *uint       { return &v }
func ptrInt64(v int64) *int64    { return &v }
func ptrInt(v int) *int          { return &v }

// resetChannelBatchTables 清空渠道与 abilities，保证批量更新用例相互隔离。
func resetChannelBatchTables(t *testing.T) {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	require.NoError(t, DB.Exec("DELETE FROM channels").Error)
}

// seedChannel 插入一条启用渠道并按其 group×models 重建 abilities 索引。
func seedChannel(t *testing.T, id int, models, group string, weight uint) {
	t.Helper()
	w := weight
	ch := &Channel{
		Id:     id,
		Name:   fmt.Sprintf("ch-%d", id),
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
		Models: models,
		Group:  group,
		Weight: &w,
	}
	require.NoError(t, DB.Create(ch).Error)
	require.NoError(t, ch.UpdateAbilities(nil))
}

// abilityModels 返回某渠道在 abilities 表中的模型集合。
func abilityModels(t *testing.T, channelId int) []string {
	t.Helper()
	var abilities []Ability
	require.NoError(t, DB.Where("channel_id = ?", channelId).Find(&abilities).Error)
	models := make([]string, 0, len(abilities))
	for _, a := range abilities {
		models = append(models, a.Model)
	}
	return models
}

// TestBatchUpdateChannelsWritesZeroValues 守护 map 形式 Updates：
// struct 形式下 weight=0 会被 GORM 静默丢弃，导致权重归零无声失效。
func TestBatchUpdateChannelsWritesZeroValues(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a", "default", 5)
	seedChannel(t, 2, "a", "default", 5)

	count, failure, err := BatchUpdateChannels([]int{1, 2}, ChannelBatchEditFields{Weight: ptrUint(0)})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, 2, count)

	var ch1, ch2 Channel
	require.NoError(t, DB.First(&ch1, 1).Error)
	require.NoError(t, DB.First(&ch2, 2).Error)
	assert.Equal(t, 0, ch1.GetWeight())
	assert.Equal(t, 0, ch2.GetWeight())
}

// TestBatchUpdateChannelsRebuildsAbilitiesOnlyOnRouteFields 路由字段变更重建
// abilities，非路由字段（remark）不触发重建。
func TestBatchUpdateChannelsRebuildsAbilitiesOnlyOnRouteFields(t *testing.T) {
	t.Run("models 生效重建 abilities", func(t *testing.T) {
		resetChannelBatchTables(t)
		seedChannel(t, 1, "a", "default", 5)
		require.Equal(t, []string{"a"}, abilityModels(t, 1))

		count, failure, err := BatchUpdateChannels([]int{1}, ChannelBatchEditFields{Models: ptrString("b,c")})
		require.NoError(t, err)
		assert.Nil(t, failure)
		assert.Equal(t, 1, count)
		assert.ElementsMatch(t, []string{"b", "c"}, abilityModels(t, 1))
	})

	t.Run("remark 生效不重建 abilities", func(t *testing.T) {
		resetChannelBatchTables(t)
		seedChannel(t, 1, "a", "default", 5)
		require.Equal(t, []string{"a"}, abilityModels(t, 1))

		count, failure, err := BatchUpdateChannels([]int{1}, ChannelBatchEditFields{Remark: ptrString("x")})
		require.NoError(t, err)
		assert.Nil(t, failure)
		assert.Equal(t, 1, count)
		assert.ElementsMatch(t, []string{"a"}, abilityModels(t, 1))
	})
}

// TestBatchUpdateChannelsSkipsMissingIds ids 中已不存在的渠道跳过，不视为失败。
func TestBatchUpdateChannelsSkipsMissingIds(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a", "default", 5)
	seedChannel(t, 2, "a", "default", 5)

	count, failure, err := BatchUpdateChannels([]int{1, 2, 999}, ChannelBatchEditFields{Weight: ptrUint(7)})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, 2, count)

	var ch1, ch2 Channel
	require.NoError(t, DB.First(&ch1, 1).Error)
	require.NoError(t, DB.First(&ch2, 2).Error)
	assert.Equal(t, 7, ch1.GetWeight())
	assert.Equal(t, 7, ch2.GetWeight())
}

// TestBatchUpdateChannelsRollsBackOnFailure 第 2 个渠道更新失败时整体回滚。
func TestBatchUpdateChannelsRollsBackOnFailure(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a", "default", 5)
	seedChannel(t, 2, "a", "default", 5)

	updateCalls := 0
	const cbName = "test:fail_on_second_update"
	require.NoError(t, DB.Callback().Update().Before("gorm:update").Register(cbName, func(tx *gorm.DB) {
		updateCalls++
		if updateCalls == 2 {
			tx.AddError(errors.New("boom"))
		}
	}))
	t.Cleanup(func() { DB.Callback().Update().Remove(cbName) })

	_, failure, err := BatchUpdateChannels([]int{1, 2}, ChannelBatchEditFields{Weight: ptrUint(0)})
	require.Error(t, err)
	require.NotNil(t, failure)
	assert.Equal(t, 2, failure.Id)
	assert.Contains(t, failure.Reason, "boom")

	var ch1 Channel
	require.NoError(t, DB.First(&ch1, 1).Error)
	assert.Equal(t, 5, ch1.GetWeight())
}

// TestBatchUpdateChannelsEmptyIds 空 ids 直接返回且不改动任何渠道。
func TestBatchUpdateChannelsEmptyIds(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a", "default", 5)

	count, failure, err := BatchUpdateChannels(nil, ChannelBatchEditFields{Weight: ptrUint(0)})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, 0, count)

	var ch1 Channel
	require.NoError(t, DB.First(&ch1, 1).Error)
	assert.Equal(t, 5, ch1.GetWeight())
}

// TestBatchUpdateChannelsNilFieldsKeepOriginal 未填写字段保持各渠道原值（规则1 空即跳过）。
func TestBatchUpdateChannelsNilFieldsKeepOriginal(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a", "default", 5)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 1).Updates(map[string]any{
		"tag":    "t1",
		"remark": "r1",
	}).Error)

	count, failure, err := BatchUpdateChannels([]int{1}, ChannelBatchEditFields{Group: ptrString("vip")})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, 1, count)

	var got Channel
	require.NoError(t, DB.First(&got, 1).Error)
	assert.Equal(t, "vip", got.Group)
	assert.Equal(t, "a", got.Models)
	assert.Equal(t, 5, got.GetWeight())
	require.NotNil(t, got.Tag)
	assert.Equal(t, "t1", *got.Tag)
	require.NotNil(t, got.Remark)
	assert.Equal(t, "r1", *got.Remark)
}

// TestChannelBatchEditFieldsAffectsAbilities 路由列判定真值表。
func TestChannelBatchEditFieldsAffectsAbilities(t *testing.T) {
	cases := []struct {
		name    string
		fields  ChannelBatchEditFields
		affects bool
	}{
		{"group", ChannelBatchEditFields{Group: ptrString("vip")}, true},
		{"tag", ChannelBatchEditFields{Tag: ptrString("t")}, true},
		{"models", ChannelBatchEditFields{Models: ptrString("a")}, true},
		{"weight", ChannelBatchEditFields{Weight: ptrUint(0)}, true},
		{"priority", ChannelBatchEditFields{Priority: ptrInt64(0)}, true},
		{"remark", ChannelBatchEditFields{Remark: ptrString("r")}, false},
		{"model_mapping", ChannelBatchEditFields{ModelMapping: ptrString("{}")}, false},
		{"test_model", ChannelBatchEditFields{TestModel: ptrString("m")}, false},
		{"auto_ban", ChannelBatchEditFields{AutoBan: ptrInt(0)}, false},
		{"none", ChannelBatchEditFields{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.affects, tc.fields.AffectsAbilities())
		})
	}
}

// TestChannelBatchEditFieldsApplyTo 列白名单 SET 与内存对象回写。
func TestChannelBatchEditFieldsApplyTo(t *testing.T) {
	fields := ChannelBatchEditFields{
		Group:        ptrString("vip"),
		Tag:          ptrString("t1"),
		Remark:       ptrString("r1"),
		Models:       ptrString("b,c"),
		ModelMapping: ptrString(`{"b":"c"}`),
		Weight:       ptrUint(0),
		Priority:     ptrInt64(0),
		TestModel:    ptrString("m1"),
		AutoBan:      ptrInt(0),
	}
	ch := &Channel{}
	got := fields.applyTo(ch)

	assert.Equal(t, map[string]any{
		"group":         "vip",
		"tag":           "t1",
		"remark":        "r1",
		"models":        "b,c",
		"model_mapping": `{"b":"c"}`,
		"weight":        uint(0),
		"priority":      int64(0),
		"test_model":    "m1",
		"auto_ban":      0,
	}, got)

	// 内存对象须同步生效值：UpdateAbilities 取自对象字段而非 DB 读。
	assert.Equal(t, "vip", ch.Group)
	assert.Equal(t, "b,c", ch.Models)
	require.NotNil(t, ch.Tag)
	assert.Equal(t, "t1", *ch.Tag)
	require.NotNil(t, ch.ModelMapping)
	assert.Equal(t, `{"b":"c"}`, *ch.ModelMapping)
	require.NotNil(t, ch.Priority)
	assert.Equal(t, int64(0), *ch.Priority)
	require.NotNil(t, ch.AutoBan)
	assert.Equal(t, 0, *ch.AutoBan)
	assert.Equal(t, 0, ch.GetWeight())
}

// TestChannelBatchEditFieldsApplyToSkipsNil 仅非 nil 字段进入 SET 白名单。
func TestChannelBatchEditFieldsApplyToSkipsNil(t *testing.T) {
	got := ChannelBatchEditFields{}.applyTo(&Channel{})
	assert.Empty(t, got)

	got = ChannelBatchEditFields{Group: ptrString("vip")}.applyTo(&Channel{})
	assert.Equal(t, map[string]any{"group": "vip"}, got)
}
