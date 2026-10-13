package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// affinityClearBinding 描述一条 token→channel 亲和绑定；model 为空时用 gpt-4。
type affinityClearBinding struct {
	tokenID   int
	channelID int
	model     string
}

// setupAffinityClearTest 初始化内存库与亲和规则（键含模型名），写入指定绑定。
func setupAffinityClearTest(t *testing.T, bindings []affinityClearBinding) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Channel{}, &model.Token{}))
	oldDB, oldLogDB, oldMemory, oldRedis := model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled
	oldSetting := *operation_setting.GetChannelAffinitySetting()
	model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled = db, db, true, false
	*operation_setting.GetChannelAffinitySetting() = operation_setting.ChannelAffinitySetting{Enabled: true, DefaultTTLSeconds: 60,
		Rules: []operation_setting.ChannelAffinityRule{{Name: "clear-test", IncludeRuleName: true, IncludeModelName: true,
			ModelRegex: []string{"^gpt-"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}}}}
	service.ClearChannelAffinityCacheAll()
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "u", Role: 1}).Error)
	channels := make(map[int]struct{}, len(bindings))
	tokens := make(map[int]struct{}, len(bindings))
	for _, b := range bindings {
		channels[b.channelID] = struct{}{}
		tokens[b.tokenID] = struct{}{}
	}
	for channelID := range channels {
		require.NoError(t, db.Create(&model.Channel{Id: channelID, Name: "渠道" + string(rune('0'+channelID))}).Error)
	}
	for tokenID := range tokens {
		require.NoError(t, db.Create(&model.Token{Id: tokenID, UserId: 1, Name: "令牌", Key: fmt.Sprintf("sk-test-%d", tokenID)}).Error)
	}
	for _, b := range bindings {
		modelName := b.model
		if modelName == "" {
			modelName = "gpt-4"
		}
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		ctx.Set("token_id", b.tokenID)
		_, _ = service.GetPreferredChannelByAffinity(ctx, modelName, "default")
		service.RecordChannelAffinity(ctx, b.channelID)
	}
	t.Cleanup(func() {
		ids := make([]int, 0, len(channels))
		for id := range channels {
			ids = append(ids, id)
		}
		service.ClearChannelAffinityRuntimeByChannelIDs(ids)
		service.ClearChannelAffinityCacheAll()
		*operation_setting.GetChannelAffinitySetting() = oldSetting
		model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled = oldDB, oldLogDB, oldMemory, oldRedis
		if sqlDB, closeErr := db.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
	})
}

func callDeleteChannelAffinityBinding(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/option/channel_affinity_bindings?"+query, nil)
	DeleteChannelAffinityBinding(c)
	return w
}

func decodeDeleteResponse(t *testing.T, w *httptest.ResponseRecorder) (bool, int) {
	t.Helper()
	var payload struct {
		Success bool `json:"success"`
		Data    struct {
			Deleted int `json:"deleted"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &payload))
	return payload.Success, payload.Data.Deleted
}

// bindingKeySuffix 从明细服务取指定 token+模型维度的键后缀。
func bindingKeySuffix(t *testing.T, channelID, tokenID int, model string) string {
	t.Helper()
	bindings, err := service.GetChannelAffinityBindings()
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ChannelID != channelID {
			continue
		}
		for _, tok := range b.Tokens {
			if tok.TokenID == tokenID && tok.ModelName == model {
				return tok.KeySuffix
			}
		}
	}
	t.Fatalf("binding not found: channel=%d token=%d model=%s", channelID, tokenID, model)
	return ""
}

func TestDeleteChannelAffinityBindingByKeySuffix(t *testing.T) {
	// 渠道 1 名下 3 条绑定：两个令牌各一条，同一令牌在另一模型下再一条
	setupAffinityClearTest(t, []affinityClearBinding{
		{tokenID: 101, channelID: 1},
		{tokenID: 102, channelID: 1},
		{tokenID: 101, channelID: 1, model: "gpt-3.5-turbo"},
		{tokenID: 103, channelID: 2},
	})

	keySuffix := bindingKeySuffix(t, 1, 101, "gpt-4")
	require.Equal(t, "clear-test:gpt-4:101", keySuffix)

	w := callDeleteChannelAffinityBinding(t, "key_suffix="+keySuffix)
	require.Equal(t, http.StatusOK, w.Code)
	success, deleted := decodeDeleteResponse(t, w)
	assert.True(t, success)
	assert.Equal(t, 1, deleted, "应只删除该 token 在该模型上的单条绑定")

	bindings, err := service.GetChannelAffinityBindings()
	require.NoError(t, err)
	require.Len(t, bindings, 2)
	for _, b := range bindings {
		switch b.ChannelID {
		case 1:
			// 渠道 1 剩 102@gpt-4 与 101@gpt-3.5
			require.Len(t, b.Tokens, 2)
			assert.Equal(t, 101, b.Tokens[0].TokenID)
			assert.Equal(t, "gpt-3.5-turbo", b.Tokens[0].ModelName)
			assert.Equal(t, 102, b.Tokens[1].TokenID)
		case 2:
			require.Len(t, b.Tokens, 1)
			assert.Equal(t, 103, b.Tokens[0].TokenID)
		}
	}
}

func TestDeleteChannelAffinityBindingByKeySuffixIdempotent(t *testing.T) {
	setupAffinityClearTest(t, []affinityClearBinding{
		{tokenID: 101, channelID: 1},
	})

	w := callDeleteChannelAffinityBinding(t, "key_suffix=clear-test:gpt-4:999")
	require.Equal(t, http.StatusOK, w.Code)
	success, deleted := decodeDeleteResponse(t, w)
	assert.True(t, success)
	assert.Zero(t, deleted, "不存在的键应幂等返回 0")

	bindings, err := service.GetChannelAffinityBindings()
	require.NoError(t, err)
	assert.Len(t, bindings, 1, "既有绑定不受影响")
}

func TestDeleteChannelAffinityBindingByKeySuffixRejectsInvalidParam(t *testing.T) {
	setupAffinityClearTest(t, []affinityClearBinding{
		{tokenID: 101, channelID: 1},
	})

	for _, query := range []string{"", "key_suffix=", "key_suffix=clear-test:gpt-4", "key_suffix=clear-test:gpt-4:abc"} {
		w := callDeleteChannelAffinityBinding(t, query)
		assert.Equal(t, http.StatusOK, w.Code, query)
		var payload struct {
			Success bool `json:"success"`
		}
		require.NoError(t, common.Unmarshal(w.Body.Bytes(), &payload), query)
		assert.False(t, payload.Success, query)
	}

	// 非法参数不得影响既有绑定
	bindings, err := service.GetChannelAffinityBindings()
	require.NoError(t, err)
	assert.Len(t, bindings, 1)
}
