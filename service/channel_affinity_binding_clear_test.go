package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 键后缀逐段回退推导候选指纹时，同渠道存在亲和值构成后缀包含关系的其它键会
// 产生多候选同时命中；无法判定归属时须保守跳过占用移除，避免误移他键成员
// （占用低计导致独占误判空闲），残留随 TTL 收敛。

func TestClearChannelAffinityBindingAmbiguousCandidatesKeepOccupancy(t *testing.T) {
	setupAffinityBindingsTest(t)
	require.NoError(t, getChannelAffinityCache().SetWithTTL("m:gpt-4:101", 931, time.Hour))
	require.NoError(t, occupancyAddKeyFP(931, affinityFingerprint("gpt-4:101"), time.Hour))
	// 他键：亲和值恰为本键去掉首段的后缀（如仅含 token 段的规则）
	require.NoError(t, occupancyAddKeyFP(931, affinityFingerprint("101"), time.Hour))

	removed, err := clearChannelAffinityBinding("m:gpt-4:101", 931, "")
	require.NoError(t, err)
	assert.True(t, removed, "正向绑定应正常删除")

	count, members, err := occupancyBindingCount(931)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "多命中时不得移除任何占用成员")
	assert.ElementsMatch(t, []string{affinityFingerprint("gpt-4:101"), affinityFingerprint("101")}, members)
}

func TestClearChannelAffinityBindingSingleHitRemovesOccupancy(t *testing.T) {
	setupAffinityBindingsTest(t)
	require.NoError(t, getChannelAffinityCache().SetWithTTL("m:gpt-4:102", 932, time.Hour))
	require.NoError(t, occupancyAddKeyFP(932, affinityFingerprint("gpt-4:102"), time.Hour))

	removed, err := clearChannelAffinityBinding("m:gpt-4:102", 932, "")
	require.NoError(t, err)
	assert.True(t, removed)

	count, _, err := occupancyBindingCount(932)
	require.NoError(t, err)
	assert.Zero(t, count, "唯一命中的占用成员应被移除")
}

func TestRemoveExclusiveOccupancyBySuffixAmbiguousKeepsMembers(t *testing.T) {
	setupAffinityBindingsTest(t)
	require.NoError(t, occupancyAddKeyFP(933, affinityFingerprint("gpt-4:103"), time.Hour))
	require.NoError(t, occupancyAddKeyFP(933, affinityFingerprint("103"), time.Hour))

	removeExclusiveOccupancyBySuffix(933, "m:gpt-4:103")

	count, members, err := occupancyBindingCount(933)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "多命中时不得移除任何占用成员")
	assert.ElementsMatch(t, []string{affinityFingerprint("gpt-4:103"), affinityFingerprint("103")}, members)
}

func TestRemoveExclusiveOccupancyBySuffixSingleHitRemoves(t *testing.T) {
	setupAffinityBindingsTest(t)
	require.NoError(t, occupancyAddKeyFP(934, affinityFingerprint("gpt-4:104"), time.Hour))

	removeExclusiveOccupancyBySuffix(934, "m:gpt-4:104")

	count, _, err := occupancyBindingCount(934)
	require.NoError(t, err)
	assert.Zero(t, count, "唯一命中的占用成员应被移除")
}
