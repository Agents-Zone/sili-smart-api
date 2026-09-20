package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 在清理读取旧绑定之后插入一次真实改绑，固定并发顺序而无需 sleep。
type affinityClearRebindHook struct {
	key    string
	fired  bool
	rebind func()
}

func (h *affinityClearRebindHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h *affinityClearRebindHook) AfterProcess(ctx context.Context, cmd redis.Cmder) error {
	if !h.fired && cmd.Name() == "get" && fmt.Sprint(cmd.Args()[1]) == h.key {
		h.fired = true
		h.rebind()
	}
	return nil
}
func (h *affinityClearRebindHook) BeforeProcessPipeline(ctx context.Context, cmds []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h *affinityClearRebindHook) AfterProcessPipeline(ctx context.Context, cmds []redis.Cmder) error {
	return nil
}

func TestClearChannelAffinityPreservesConcurrentRebind(t *testing.T) {
	for _, mode := range []string{"redis", "realredis"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "realredis" {
				useRealRedisMode(t)
			} else {
				useExclusiveRedisMode(t)
			}
			useSoftPlacementFixture(t, true)
			const key = "clear-rebind"
			seedExclusiveBinding(t, key, 921)
			require.NoError(t, occupancyAddKeyFP(921, affinityFingerprint("other-holder"), time.Minute))
			moved := 0
			hook := &affinityClearRebindHook{key: getChannelAffinityCache().FullKey(affinityKeySuffix(key))}
			hook.rebind = func() {
				ctx, _ := raceHitCtx(key, 921)
				moved = rebalanceExclusiveBinding(ctx, 921, "default", "gpt-4")
				require.NotEqual(t, 921, moved)
				require.Equal(t, moved, forwardBinding(t, key))
			}
			common.RDB.AddHook(hook)
			deleted := ClearChannelAffinityRuntimeByChannelIDs([]int{921})
			require.True(t, hook.fired)
			assert.Zero(t, deleted, "已迁到其它渠道的绑定不计入删除数")
			assert.Equal(t, moved, forwardBinding(t, key))
			record, found, err := lastBindGet(affinityKeySuffix(key))
			require.NoError(t, err)
			assert.True(t, found)
			assert.Equal(t, moved, record.ChannelID)
			assert.Equal(t, 1, countKeyFingerprintAcross(t, key))
		})
	}
}

func TestClearChannelAffinityRemovesHistoryWithoutForward(t *testing.T) {
	for _, mode := range []string{"memory", "redis", "realredis"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "realredis" {
				useRealRedisMode(t)
			} else if mode == "redis" {
				useExclusiveRedisMode(t)
			} else {
				useExclusiveMemoryMode(t)
			}
			useSoftPlacementFixture(t, true)
			seedExclusiveBinding(t, "expired", 921)
			seedExclusiveBinding(t, "other", 922)
			_, err := getChannelAffinityCache().DeleteMany([]string{affinityKeySuffix("expired")})
			require.NoError(t, err)
			assert.Zero(t, ClearChannelAffinityRuntimeByChannelIDs([]int{921}))
			_, found, err := lastBindGet(affinityKeySuffix("expired"))
			require.NoError(t, err)
			assert.False(t, found)
			assert.Zero(t, channelMemberCount(t, 921))
			assert.Equal(t, 922, forwardBinding(t, "other"))
		})
	}
}

func TestExclusiveAffinityRebindsDeletedChannel(t *testing.T) {
	for _, mode := range []string{"memory", "redis", "realredis"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "realredis" {
				useRealRedisMode(t)
			} else if mode == "redis" {
				useExclusiveRedisMode(t)
			} else {
				useExclusiveMemoryMode(t)
			}
			useSoftPlacementFixture(t, true)
			seedExclusiveBinding(t, "deleted", 921)
			require.NoError(t, (&model.Channel{Id: 921}).Delete())
			model.InitChannelCache()
			id, found := GetPreferredChannelByAffinity(newAffinityRequestCtx("deleted"), "gpt-4", "default")
			require.True(t, found)
			assert.NotEqual(t, 921, id)
			assert.Zero(t, channelMemberCount(t, 921))
			assert.Equal(t, id, forwardBinding(t, "deleted"))
		})
	}
}

func TestExclusiveAffinityLookupFailurePreservesBinding(t *testing.T) {
	useExclusiveMemoryMode(t)
	useSoftPlacementFixture(t, true)
	seedExclusiveBinding(t, "lookup-error", 921)
	previous := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previous })
	expected := errors.New("测试数据库查询故障")
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register("affinity_query_error", func(db *gorm.DB) { db.AddError(expected) }))
	t.Cleanup(func() { model.DB.Callback().Query().Remove("affinity_query_error") })
	_, meta := raceHitCtx("lookup-error", 921)
	released, err := releaseDisabledAffinityBinding(meta, 921)
	assert.ErrorIs(t, err, expected)
	assert.False(t, released)
	assert.Equal(t, 921, forwardBinding(t, "lookup-error"))
	assert.Equal(t, 1, channelMemberCount(t, 921))
}
