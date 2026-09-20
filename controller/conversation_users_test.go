package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupConversationUsersControllerTestDB 初始化 controller 用户信息字典测试夹具：
// 内存 SQLite 作为主库与日志库，AutoMigrate users/tokens 表，并初始化 i18n
// 使 common.ApiErrorI18n 输出翻译后的消息。模式照搬 setupConversationControllerTestDB
// （快照全局、gin.TestMode、SetDatabaseTypes、RedisEnabled=false、cleanup 恢复）。
func setupConversationUsersControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	previousMainDatabaseType, previousLogDatabaseType := common.MainDatabaseType(), common.LogDatabaseType()

	gin.SetMode(gin.TestMode)
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	require.NoError(t, i18n.Init())

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	model.LOG_DB = db
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}))

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled = previousRedisEnabled
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// conversationUsersPayload 解析用户信息字典列表响应。
type conversationUsersPayload struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
		Total    int `json:"total"`
		Items    []struct {
			UserID   int    `json:"user_id"`
			Username string `json:"username"`
			Tokens   []struct {
				TokenID   int    `json:"token_id"`
				TokenName string `json:"token_name"`
			} `json:"tokens"`
		} `json:"items"`
	} `json:"data"`
}

// newConvTestUser 构造启用/禁用用户，填入唯一 AffCode 规避 uniqueIndex 空值冲突。
func newConvTestUser(id int, username string, status int) *model.User {
	return &model.User{Id: id, Username: username, Status: status, AffCode: "aff-" + fmt.Sprint(id)}
}

// newConvTestToken 构造 token，填入唯一 Key 规避 uniqueIndex 空值冲突。
func newConvTestToken(id, userId int, name string, status int) *model.Token {
	return &model.Token{Id: id, UserId: userId, Name: name, Status: status, Key: "key-" + fmt.Sprint(id)}
}

// TestListConversationUsersSuccess 成功响应：聚合返回用户与其名下启用 token，
// 分页字段回传 query 指定的 page_size，data 为 {page,page_size,total,items}。
func TestListConversationUsersSuccess(t *testing.T) {
	db := setupConversationUsersControllerTestDB(t)
	require.NoError(t, db.Create(newConvTestUser(2, "user1", common.UserStatusEnabled)).Error)
	require.NoError(t, db.Create(newConvTestToken(10, 2, "tok-a", common.UserStatusEnabled)).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?username=user&page_size=20", nil)

	ListConversationUsers(ctx)

	var payload conversationUsersPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.True(t, payload.Success)
	assert.Equal(t, 1, payload.Data.Page)
	assert.Equal(t, 20, payload.Data.PageSize)
	assert.Equal(t, 1, payload.Data.Total)
	require.Len(t, payload.Data.Items, 1)
	assert.Equal(t, 2, payload.Data.Items[0].UserID)
	assert.Equal(t, "user1", payload.Data.Items[0].Username)
	require.Len(t, payload.Data.Items[0].Tokens, 1)
	assert.Equal(t, 10, payload.Data.Items[0].Tokens[0].TokenID)
	assert.Equal(t, "tok-a", payload.Data.Items[0].Tokens[0].TokenName)
}

// TestListConversationUsersNoMatch 无满足条件用户：200 空 items（total=0），
// items 为空数组而非 null。
func TestListConversationUsersNoMatch(t *testing.T) {
	db := setupConversationUsersControllerTestDB(t)
	require.NoError(t, db.Create(newConvTestUser(2, "user1", common.UserStatusEnabled)).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?username=nonexistent", nil)

	ListConversationUsers(ctx)

	var payload conversationUsersPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.True(t, payload.Success)
	assert.Equal(t, 0, payload.Data.Total)
	assert.Empty(t, payload.Data.Items)
}

// TestListConversationUsersFieldMinimization 响应字段最小化：结构上不泄露
// password/email/key 等敏感字段值（SSOT §3.2）。
func TestListConversationUsersFieldMinimization(t *testing.T) {
	db := setupConversationUsersControllerTestDB(t)
	require.NoError(t, db.Create(&model.User{
		Id:       2,
		Username: "u1",
		Password: "s3cr3t-pw",
		Email:    "h1dden@x.com",
		Status:   common.UserStatusEnabled,
		AffCode:  "aff-2",
	}).Error)
	require.NoError(t, db.Create(&model.Token{
		Id:     10,
		UserId: 2,
		Name:   "tok-a",
		Key:    "sk-h1dden",
		Status: common.UserStatusEnabled,
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	ListConversationUsers(ctx)

	body := recorder.Body.String()
	assert.NotContains(t, body, "s3cr3t-pw", "响应不得包含 password 值")
	assert.NotContains(t, body, "h1dden@x.com", "响应不得包含 email 值")
	assert.NotContains(t, body, "sk-h1dden", "响应不得包含 token key 值")
}

// TestListConversationUsersDefaultPagination 无分页参数走 GetPageQuery 默认值（page=1，
// page_size 默认 ItemsPerPage），验证查询参数全可选容错。
func TestListConversationUsersDefaultPagination(t *testing.T) {
	db := setupConversationUsersControllerTestDB(t)
	require.NoError(t, db.Create(newConvTestUser(2, "user1", common.UserStatusEnabled)).Error)
	require.NoError(t, db.Create(newConvTestToken(10, 2, "tok-a", common.UserStatusEnabled)).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	ListConversationUsers(ctx)

	var payload conversationUsersPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	assert.True(t, payload.Success)
	assert.Equal(t, 1, payload.Data.Page)
	assert.Greater(t, payload.Data.PageSize, 0, "默认 page_size 由 GetPageQuery 兜底")
	assert.Equal(t, 1, payload.Data.Total)
}

// TestListConversationUsersTokenNameFilter token_name 过滤：仅返回名下含匹配 token 的用户，
// 且该用户 tokens 只含匹配项（验证 TokenName 参数正确组装到 model 查询）。
func TestListConversationUsersTokenNameFilter(t *testing.T) {
	db := setupConversationUsersControllerTestDB(t)
	require.NoError(t, db.Create(newConvTestUser(2, "user1", common.UserStatusEnabled)).Error)
	require.NoError(t, db.Create(newConvTestToken(10, 2, "prod-key", common.UserStatusEnabled)).Error)
	require.NoError(t, db.Create(newConvTestToken(11, 2, "dev-key", common.UserStatusEnabled)).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?token_name=prod", nil)

	ListConversationUsers(ctx)

	var payload conversationUsersPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	assert.True(t, payload.Success)
	assert.Equal(t, 1, payload.Data.Total)
	require.Len(t, payload.Data.Items, 1)
	require.Len(t, payload.Data.Items[0].Tokens, 1, "仅返回匹配 prod 的 token")
	assert.Equal(t, "prod-key", payload.Data.Items[0].Tokens[0].TokenName)
}

// TestListConversationUsersDatabaseError 查询失败返回 success:false 与 MsgDatabaseError
// （与会话列表 ListConversations 一致，SSOT §2.4 注意事项）。通过 DropTable users 触发
// model.ListConversationUsers 的用户查询错误。
func TestListConversationUsersDatabaseError(t *testing.T) {
	db := setupConversationUsersControllerTestDB(t)
	require.NoError(t, db.Migrator().DropTable("users"))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	ListConversationUsers(ctx)

	var payload conversationUsersPayload
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	assert.False(t, payload.Success)
}
