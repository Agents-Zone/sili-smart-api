package controller

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestDeleteChannelClearsAffinity(t *testing.T) {
	for _, mode := range []string{"single", "batch", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			require.NoError(t, i18n.Init())
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}, &model.Log{}))
			oldDB, oldLogDB, oldMemory, oldRedis := model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled
			setting := operation_setting.GetChannelAffinitySetting()
			oldSetting := *setting
			model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled = db, db, true, false
			*setting = operation_setting.ChannelAffinitySetting{Enabled: true, DefaultTTLSeconds: 60,
				Rules: []operation_setting.ChannelAffinityRule{{Name: t.Name(), IncludeRuleName: true, ExclusiveBind: true,
					ModelRegex: []string{"^gpt-"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "context_int", Key: "token_id"}}}}}
			t.Cleanup(func() {
				service.ClearChannelAffinityRuntimeByChannelIDs([]int{9801, 9802})
				*setting = oldSetting
				model.DB, model.LOG_DB, common.MemoryCacheEnabled, common.RedisEnabled = oldDB, oldLogDB, oldMemory, oldRedis
				if oldMemory && oldDB != nil {
					model.InitChannelCache()
				}
				sqlDB, err := db.DB()
				require.NoError(t, err)
				require.NoError(t, sqlDB.Close())
			})
			for _, id := range []int{9801, 9802} {
				require.NoError(t, db.Create(&model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled,
					Key: "test", Models: "gpt-4", Group: "default"}).Error)
				require.NoError(t, db.Create(&model.Ability{ChannelId: id, Group: "default", Model: "gpt-4", Enabled: true}).Error)
			}
			model.InitChannelCache()
			selected := 0
			engine := gin.New()
			engine.POST("/v1/chat/completions", func(c *gin.Context) {
				c.Set("token_id", 234567)
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
			}, middleware.Distribute(), func(c *gin.Context) { selected = c.GetInt("channel_id"); c.Status(http.StatusOK) })
			request := func() int {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4"}`))
				r.Header.Set("Content-Type", "application/json")
				engine.ServeHTTP(w, r)
				return w.Code
			}
			require.Equal(t, 200, request())
			deletedID := selected
			w := httptest.NewRecorder()
			admin, _ := gin.CreateTestContext(w)
			admin.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/", nil)
			switch mode {
			case "single":
				admin.Params = gin.Params{{Key: "id", Value: strconv.Itoa(deletedID)}}
				DeleteChannel(admin)
			case "batch":
				admin.Request = httptest.NewRequest(http.MethodPost, "/api/channel/batch", strings.NewReader(`{"ids":[`+strconv.Itoa(deletedID)+`,999999]}`))
				admin.Request.Header.Set("Content-Type", "application/json")
				DeleteChannelBatch(admin)
			case "disabled":
				require.True(t, model.UpdateChannelStatus(deletedID, "", common.ChannelStatusManuallyDisabled, "测试禁用"))
				DeleteDisabledChannel(admin)
			}
			require.Equal(t, 200, w.Code)
			var response struct {
				Success bool `json:"success"`
			}
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &response))
			require.True(t, response.Success)
			_, err = model.GetChannelById(deletedID, false)
			require.ErrorIs(t, err, gorm.ErrRecordNotFound)
			// 在下一请求触发兜底前检查，保证删除入口本身已完成清理。
			exclusive, shared := service.GetChannelAffinityExclusiveStats()
			assert.Zero(t, exclusive+shared)
			assert.Equal(t, 200, request())
			assert.NotEqual(t, deletedID, selected)
		})
	}
}
