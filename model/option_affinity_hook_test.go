package model

import (
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 开关联动钩子在数据库加载阶段（启动首扫/周期同步）必须跳过：启动首扫时内存配置
// 是内置默认（ExclusiveBind 全 false），与库值比对会把"进程重启"误判为"开关变化"，
// 错误触发独占运行时清空（多节点部署下单节点重启即清空全集群）。
func TestRulesExclusiveBindHookSkippedDuringDBLoad(t *testing.T) {
	// 保存前把内存 rules 预置为空（对齐另一进程刚启动、尚未从库加载的状态）。
	cfg := config.GlobalConfig.Get("channel_affinity_setting")
	require.NotNil(t, cfg)
	affinityCfg, ok := cfg.(*operation_setting.ChannelAffinitySetting)
	require.True(t, ok)
	prevRules := affinityCfg.Rules
	affinityCfg.Rules = nil
	t.Cleanup(func() { affinityCfg.Rules = prevRules })

	var hookCalls int32
	prevHook := operation_setting.OnRulesExclusiveBindChanged
	operation_setting.OnRulesExclusiveBindChanged = func(oldJSON, newJSON string) {
		atomic.AddInt32(&hookCalls, 1)
	}
	t.Cleanup(func() { operation_setting.OnRulesExclusiveBindChanged = prevHook })

	// 数据库加载阶段（标记置位）：差异比对也不触发钩子。
	optionsLoadedFromDB.Store(true)
	t.Cleanup(func() { optionsLoadedFromDB.Store(false) })
	assert.True(t, handleConfigUpdate("channel_affinity_setting.rules",
		`[{"name":"r1","exclusive_bind":true}]`))
	assert.Equal(t, int32(0), atomic.LoadInt32(&hookCalls), "hook must not fire during DB load")

	// 用户保存路径（标记复位）：开关变化正常触发钩子。
	optionsLoadedFromDB.Store(false)
	assert.True(t, handleConfigUpdate("channel_affinity_setting.rules",
		`[{"name":"r1","exclusive_bind":false}]`))
	assert.Equal(t, int32(1), atomic.LoadInt32(&hookCalls), "hook must fire on user save")
}
