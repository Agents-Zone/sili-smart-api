package model

import (
	"errors"
	"fmt"
	"strings"
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

	updatedIds, failure, err := BatchUpdateChannels([]int{1, 2}, ChannelBatchEditFields{Weight: ptrUint(0)})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, []int{1, 2}, updatedIds)

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

		updatedIds, failure, err := BatchUpdateChannels([]int{1}, ChannelBatchEditFields{Models: ptrString("b,c")})
		require.NoError(t, err)
		assert.Nil(t, failure)
		assert.Equal(t, []int{1}, updatedIds)
		assert.ElementsMatch(t, []string{"b", "c"}, abilityModels(t, 1))
	})

	t.Run("remark 生效不重建 abilities", func(t *testing.T) {
		resetChannelBatchTables(t)
		seedChannel(t, 1, "a", "default", 5)
		require.Equal(t, []string{"a"}, abilityModels(t, 1))

		updatedIds, failure, err := BatchUpdateChannels([]int{1}, ChannelBatchEditFields{Remark: ptrString("x")})
		require.NoError(t, err)
		assert.Nil(t, failure)
		assert.Equal(t, []int{1}, updatedIds)
		assert.ElementsMatch(t, []string{"a"}, abilityModels(t, 1))
	})
}

// TestBatchUpdateChannelsSkipsMissingIds ids 中已不存在的渠道跳过，不视为失败。
func TestBatchUpdateChannelsSkipsMissingIds(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a", "default", 5)
	seedChannel(t, 2, "a", "default", 5)

	updatedIds, failure, err := BatchUpdateChannels([]int{1, 2, 999}, ChannelBatchEditFields{Weight: ptrUint(7)})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, []int{1, 2}, updatedIds)

	var ch1, ch2 Channel
	require.NoError(t, DB.First(&ch1, 1).Error)
	require.NoError(t, DB.First(&ch2, 2).Error)
	assert.Equal(t, 7, ch1.GetWeight())
	assert.Equal(t, 7, ch2.GetWeight())
}

// TestBatchUpdateChannelsRollsBackOnFailure 事务中任一写库语句失败即整体回滚：
// 第 2 个 UPDATE（渠道 2）失败即回滚，失败明细精确指向该渠道。
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

	updatedIds, failure, err := BatchUpdateChannels([]int{1, 2}, ChannelBatchEditFields{Weight: ptrUint(0)})
	require.Error(t, err)
	assert.Empty(t, updatedIds)
	require.NotNil(t, failure)
	// 失败明细指向触发回滚的渠道，而非整批的首个渠道
	assert.Equal(t, 2, failure.Id)
	assert.Equal(t, BatchFailureUpdateChannels, failure.Kind)
	require.Error(t, failure.Err)
	assert.Contains(t, failure.Err.Error(), "boom")

	for _, id := range []int{1, 2} {
		var ch Channel
		require.NoError(t, DB.First(&ch, id).Error)
		assert.Equal(t, 5, ch.GetWeight())
	}
}

// TestBatchUpdateChannelsEmptyIds 空 ids 直接返回且不改动任何渠道。
func TestBatchUpdateChannelsEmptyIds(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a", "default", 5)

	updatedIds, failure, err := BatchUpdateChannels(nil, ChannelBatchEditFields{Weight: ptrUint(0)})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Empty(t, updatedIds)

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

	updatedIds, failure, err := BatchUpdateChannels([]int{1}, ChannelBatchEditFields{Group: ptrString("vip")})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, []int{1}, updatedIds)

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

// TestChannelBatchEditFieldsColumnValuesAndAssign 列白名单 SET 与内存对象回写同源：
// 同一个字段访问器产出写入值，也把生效值落回渠道对象。
func TestChannelBatchEditFieldsColumnValuesAndAssign(t *testing.T) {
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
	}, fields.ColumnValues())

	ch := &Channel{}
	fields.assignTo(ch)

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

// TestChannelBatchEditFieldsColumnValuesSkipsNil 仅非 nil 字段进入 SET 白名单与回写。
func TestChannelBatchEditFieldsColumnValuesSkipsNil(t *testing.T) {
	ch := &Channel{Group: "default"}
	assert.Empty(t, ChannelBatchEditFields{}.ColumnValues())
	ChannelBatchEditFields{}.assignTo(ch)
	assert.Equal(t, "default", ch.Group)

	groupOnly := ChannelBatchEditFields{Group: ptrString("vip")}
	assert.Equal(t, map[string]any{"group": "vip"}, groupOnly.ColumnValues())
	groupOnly.assignTo(ch)
	assert.Equal(t, "vip", ch.Group)
}

// TestBatchUpdateChannelsKeepsAbilitySegmentsClean 段值原样落库时 abilities 行
// 与 UpdateAbilities 的裸 split 语义对齐（归一化在 controller 校验层完成）。
func TestBatchUpdateChannelsKeepsAbilitySegmentsClean(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a", "default", 5)

	_, failure, err := BatchUpdateChannels([]int{1}, ChannelBatchEditFields{
		Models: ptrString("b,c"),
		Group:  ptrString("vip,default"),
	})
	require.NoError(t, err)
	assert.Nil(t, failure)

	var ch Channel
	require.NoError(t, DB.First(&ch, 1).Error)
	assert.Equal(t, "b,c", ch.Models)
	assert.Equal(t, "vip,default", ch.Group)

	var abilities []Ability
	require.NoError(t, DB.Where("channel_id = ?", 1).Find(&abilities).Error)
	// 2 模型 × 2 分组的笛卡尔积
	require.Len(t, abilities, 4)
	for _, ability := range abilities {
		assert.Equal(t, strings.TrimSpace(ability.Model), ability.Model)
		assert.Equal(t, strings.TrimSpace(ability.Group), ability.Group)
	}
}

// TestBatchUpdateChannelsRebuildsAbilityColumns 仅改行内列（weight/priority/tag）时
// 重建后的 abilities 行集不变、行内列取到新值：模型集合保持原样，weight 落到 ability 行。
func TestBatchUpdateChannelsRebuildsAbilityColumns(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a,b", "default", 5)
	seedChannel(t, 2, "a,b", "default", 5)

	updatedIds, failure, err := BatchUpdateChannels([]int{1, 2}, ChannelBatchEditFields{Weight: ptrUint(9)})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.Equal(t, []int{1, 2}, updatedIds)

	// (group,model) 行集不变：每渠道仍是 default×{a,b}
	assert.ElementsMatch(t, []string{"a", "b"}, abilityModels(t, 1))

	var abilities []Ability
	require.NoError(t, DB.Where("channel_id = ?", 1).Find(&abilities).Error)
	require.Len(t, abilities, 2)
	for _, ability := range abilities {
		assert.Equal(t, uint(9), ability.Weight)
	}
}

// TestBatchUpdateChannelsRepairsDriftedAbilities 路由字段生效时按渠道删除重插：
// 渠道行与 abilities 漂移（缺行）时批量编辑可自愈，与单渠道编辑口径一致。
func TestBatchUpdateChannelsRepairsDriftedAbilities(t *testing.T) {
	resetChannelBatchTables(t)
	seedChannel(t, 1, "a,b", "default", 5)
	// 模拟 channels 行已写入但 abilities 缺行的漂移状态
	require.NoError(t, DB.Exec("DELETE FROM abilities WHERE channel_id = ? AND model = ?", 1, "b").Error)
	require.Equal(t, []string{"a"}, abilityModels(t, 1))

	_, failure, err := BatchUpdateChannels([]int{1}, ChannelBatchEditFields{Weight: ptrUint(9)})
	require.NoError(t, err)
	assert.Nil(t, failure)
	assert.ElementsMatch(t, []string{"a", "b"}, abilityModels(t, 1))
}

// TestChannelBatchEditFieldsDerivedFromSingleSource 审计派生方法与 SET 白名单同源：
// 列集合一致，值经解引用。
func TestChannelBatchEditFieldsDerivedFromSingleSource(t *testing.T) {
	fields := ChannelBatchEditFields{
		Remark: ptrString("r1"),
		Weight: ptrUint(0),
	}
	assert.Equal(t, []string{"remark", "weight"}, fields.EffectiveColumns())
	assert.Equal(t, map[string]any{"remark": "r1", "weight": uint(0)}, fields.ColumnValues())
}
