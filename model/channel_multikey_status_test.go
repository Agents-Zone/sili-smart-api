package model

// 多密钥渠道自动禁用的回归测试：守护"关键词命中后出错 key 被禁、
// 其余 key 不受影响、全禁后渠道整体禁用、被禁 key 不再被选中"的完整状态机。

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetMultiKeyStatusTables(t *testing.T) {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	require.NoError(t, DB.Exec("DELETE FROM channels").Error)
}

func seedMultiKeyChannel(t *testing.T, id int, keys string) *Channel {
	t.Helper()
	autoBan := 1
	ch := &Channel{
		Id:     id,
		Name:   "mk-ch",
		Key:    keys,
		Status: common.ChannelStatusEnabled,
		Models: "gpt-test",
		Group:  "default",
		AutoBan: &autoBan,
		ChannelInfo: ChannelInfo{
			IsMultiKey:   true,
			MultiKeyMode: constant.MultiKeyModePolling,
		},
	}
	require.NoError(t, DB.Create(ch).Error)
	return ch
}

// 场景 1：余额不足命中后，仅禁用出错的那个 key，渠道整体保持启用。
func TestMKAutoDisableOnlyFailedKey(t *testing.T) {
	resetMultiKeyStatusTables(t)
	ch := seedMultiKeyChannel(t, 101, "sk-a\nsk-b")

	ok := UpdateChannelStatus(ch.Id, "sk-a", common.ChannelStatusAutoDisabled, "余额不足")
	require.True(t, ok)

	after, err := GetChannelById(ch.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, after.Status, "还剩 sk-b，渠道整体应保持启用")
	require.NotNil(t, after.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, common.ChannelStatusAutoDisabled, after.ChannelInfo.MultiKeyStatusList[0], "sk-a 应被标记为自动禁用")
	assert.NotContains(t, after.ChannelInfo.MultiKeyStatusList, 1, "sk-b 不应被禁用")
}

// 场景 2：所有 key 依次被禁后，渠道整体应变为自动禁用。
func TestMKAutoDisableAllKeysDisablesChannel(t *testing.T) {
	resetMultiKeyStatusTables(t)
	ch := seedMultiKeyChannel(t, 102, "sk-a\nsk-b")

	require.True(t, UpdateChannelStatus(ch.Id, "sk-a", common.ChannelStatusAutoDisabled, "余额不足"))
	require.True(t, UpdateChannelStatus(ch.Id, "sk-b", common.ChannelStatusAutoDisabled, "余额不足"))

	after, err := GetChannelById(ch.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, after.Status, "全部 key 禁用后渠道整体应禁用")
}

// 场景 3：usingKey 与渠道 key 列表全等不匹配时，静默跳过：状态完全不变但返回 true。
// 该行为是已知缺陷：调用方（DisableChannel）以为禁用成功，实际被丢弃，仅有一条 SysLog。
func TestMKAutoDisableUsingKeyMismatch(t *testing.T) {
	resetMultiKeyStatusTables(t)
	ch := seedMultiKeyChannel(t, 103, "sk-a\nsk-b")

	// DB 中 key 实际带 \r（CRLF 粘贴），而失败请求传入的是干净字符串
	ch.Key = "sk-a\r\nsk-b\r\n"
	require.NoError(t, DB.Save(ch).Error)

	ok := UpdateChannelStatus(ch.Id, "sk-a", common.ChannelStatusAutoDisabled, "余额不足")
	after, err := GetChannelById(ch.Id, true)
	require.NoError(t, err)
	assert.Empty(t, after.ChannelInfo.MultiKeyStatusList, "usingKey 不匹配时禁用被静默丢弃")
	assert.Equal(t, common.ChannelStatusEnabled, after.Status)
	assert.True(t, ok, "返回值仍为 true，调用方无从感知失败")
}

// 场景 4：内存缓存开启时，命中关键词后 key 禁用必须落到缓存与 DB 两处。
func TestMKAutoDisableWithMemoryCache(t *testing.T) {
	resetMultiKeyStatusTables(t)
	ch := seedMultiKeyChannel(t, 104, "sk-a\nsk-b")
	require.NoError(t, ch.UpdateAbilities(nil))

	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = false })
	InitChannelCache()

	ok := UpdateChannelStatus(ch.Id, "sk-a", common.ChannelStatusAutoDisabled, "余额不足")
	require.True(t, ok)

	// DB 断言
	after, err := GetChannelById(ch.Id, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, after.Status)
	require.NotNil(t, after.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, common.ChannelStatusAutoDisabled, after.ChannelInfo.MultiKeyStatusList[0],
		"内存缓存开启时，key 禁用必须持久化到 DB")

	// 缓存断言
	cached, err := CacheGetChannel(ch.Id)
	require.NoError(t, err)
	require.NotNil(t, cached.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached.ChannelInfo.MultiKeyStatusList[0],
		"缓存中的 key 也应被标记禁用")

	// 缓存重建后状态仍保留（模拟 SyncChannelCache 周期重建）
	InitChannelCache()
	cached2, err := CacheGetChannel(ch.Id)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, cached2.ChannelInfo.MultiKeyStatusList[0],
		"缓存重建后禁用状态应从 DB 恢复")
}

// 场景 5：key 被禁后，GetNextEnabledKey 必须跳过它；全禁后报无可用 key。
func TestMKGetNextEnabledKeySkipsDisabled(t *testing.T) {
	resetMultiKeyStatusTables(t)
	ch := seedMultiKeyChannel(t, 105, "sk-a\nsk-b")

	require.True(t, UpdateChannelStatus(ch.Id, "sk-a", common.ChannelStatusAutoDisabled, "余额不足"))

	after, err := GetChannelById(ch.Id, true)
	require.NoError(t, err)
	t.Logf("现场: key=%q statusList=%v polling=%d mode=%q isMultiKey=%v cache=%v",
		after.Key, after.ChannelInfo.MultiKeyStatusList, after.ChannelInfo.MultiKeyPollingIndex, after.ChannelInfo.MultiKeyMode, after.ChannelInfo.IsMultiKey, common.MemoryCacheEnabled)
	key, _, keyErr := after.GetNextEnabledKey()
	if keyErr != nil {
		t.Fatalf("keyErr 详情: %#v / %q", keyErr, keyErr.Error())
	}
	assert.Equal(t, "sk-b", key, "被禁的 sk-a 不应再被选中")

	// 全禁后应返回无可用 key
	require.True(t, UpdateChannelStatus(ch.Id, "sk-b", common.ChannelStatusAutoDisabled, "余额不足"))
	after2, err := GetChannelById(ch.Id, true)
	require.NoError(t, err)
	_, _, keyErr = after2.GetNextEnabledKey()
	require.Error(t, keyErr)
}
