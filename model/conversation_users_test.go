package model

import (
	"strconv"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupConversationUsersTestDB 确保 users/tokens 表存在，并在测试结束后清空，
// 复用 model 包 TestMain 的共享内存 DB。
func setupConversationUsersTestDB(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&User{}, &Token{}))
	t.Cleanup(func() {
		DB.Exec("DELETE FROM users")
		DB.Exec("DELETE FROM tokens")
	})
}

// newTestUser 构造启用/禁用用户，填入唯一 AffCode 规避 uniqueIndex 空值冲突。
func newTestUser(id int, username string, status int) *User {
	return &User{Id: id, Username: username, Status: status, AffCode: "aff-" + strconv.Itoa(id)}
}

// newTestToken 构造 token，填入唯一 Key 规避 uniqueIndex 空值冲突。
func newTestToken(id, userId int, name string, status int) *Token {
	return &Token{Id: id, UserId: userId, Name: name, Status: status, Key: "key-" + strconv.Itoa(id)}
}

func TestListConversationUsersAggregatesByUser(t *testing.T) {
	setupConversationUsersTestDB(t)
	require.NoError(t, DB.Create(newTestUser(2, "user1", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(10, 2, "my-token", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(11, 2, "prod-key", common.UserStatusEnabled)).Error)

	result, total, err := ListConversationUsers(&ConversationUserQueryParams{}, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, result, 1)
	assert.Equal(t, 2, result[0].UserID)
	assert.Equal(t, "user1", result[0].Username)
	require.Len(t, result[0].Tokens, 2)
	assert.Equal(t, ConversationToken{TokenID: 10, TokenName: "my-token"}, result[0].Tokens[0])
	assert.Equal(t, ConversationToken{TokenID: 11, TokenName: "prod-key"}, result[0].Tokens[1])
}

func TestListConversationUsersOnlyEnabled(t *testing.T) {
	setupConversationUsersTestDB(t)
	require.NoError(t, DB.Create(newTestUser(2, "user1", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestUser(3, "user2", common.UserStatusDisabled)).Error)
	// 启用 token 归属 user2
	require.NoError(t, DB.Create(newTestToken(10, 2, "my-token", common.UserStatusEnabled)).Error)
	// 禁用 token，应排除
	require.NoError(t, DB.Create(newTestToken(11, 2, "disabled-token", common.UserStatusDisabled)).Error)
	// 软删除 token，应排除（先创建再 Delete 触发 GORM 软删除）
	require.NoError(t, DB.Create(newTestToken(12, 2, "deleted-token", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Delete(&Token{Id: 12}).Error)

	result, total, err := ListConversationUsers(&ConversationUserQueryParams{}, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, result, 1)
	assert.Equal(t, 2, result[0].UserID)
	// user2(Status:disabled) 被排除，仅剩 user1
	assert.Equal(t, "user1", result[0].Username)
	// 禁用 token(11) 与软删除 token(12) 均排除，仅剩启用 token(10)
	require.Len(t, result[0].Tokens, 1)
	assert.Equal(t, 10, result[0].Tokens[0].TokenID)
	assert.Equal(t, "my-token", result[0].Tokens[0].TokenName)
}

func TestListConversationUsersEnabledUserWithoutEnabledTokens(t *testing.T) {
	setupConversationUsersTestDB(t)
	require.NoError(t, DB.Create(newTestUser(2, "user1", common.UserStatusEnabled)).Error)
	// 名下 token 全部禁用
	require.NoError(t, DB.Create(newTestToken(20, 2, "disabled", common.UserStatusDisabled)).Error)
	// 名下一个软删除 token
	require.NoError(t, DB.Create(newTestToken(21, 2, "deleted", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Delete(&Token{Id: 21}).Error)

	result, total, err := ListConversationUsers(&ConversationUserQueryParams{}, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, result, 1)
	assert.Equal(t, 2, result[0].UserID)
	// LEFT JOIN 语义：启用用户仍返回，tokens 为空数组非 nil
	require.Len(t, result[0].Tokens, 0)
	assert.NotNil(t, result[0].Tokens)
}

func TestListConversationUsersUsernameFilter(t *testing.T) {
	setupConversationUsersTestDB(t)
	require.NoError(t, DB.Create(newTestUser(2, "alice", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestUser(3, "bob", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestUser(4, "carol", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(10, 2, "a-key", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(11, 4, "c-key", common.UserStatusEnabled)).Error)

	result, total, err := ListConversationUsers(&ConversationUserQueryParams{Username: "o"}, 1, 10)
	require.NoError(t, err)
	// username 含 "o"：bob、carol
	assert.Equal(t, int64(2), total)
	require.Len(t, result, 2)
	assert.Equal(t, 3, result[0].UserID)
	assert.Equal(t, "bob", result[0].Username)
	assert.Equal(t, 4, result[1].UserID)
	assert.Equal(t, "carol", result[1].Username)
}

func TestListConversationUsersTokenNameFilter(t *testing.T) {
	setupConversationUsersTestDB(t)
	// user2 同时拥有 prod-key 与 dev-key
	require.NoError(t, DB.Create(newTestUser(2, "user1", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(10, 2, "prod-key", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(11, 2, "dev-key", common.UserStatusEnabled)).Error)
	// user3 也拥有 prod-key
	require.NoError(t, DB.Create(newTestUser(3, "user2", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(12, 3, "prod-key", common.UserStatusEnabled)).Error)
	// user4 仅拥有不匹配的 token，应在用户收口阶段被排除
	require.NoError(t, DB.Create(newTestUser(4, "user3", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(13, 4, "other-key", common.UserStatusEnabled)).Error)

	result, total, err := ListConversationUsers(&ConversationUserQueryParams{TokenName: "prod"}, 1, 10)
	require.NoError(t, err)
	// 仅保留名下至少含一个匹配 "prod" token 的用户：user2、user3（user4 排除）
	assert.Equal(t, int64(2), total)
	require.Len(t, result, 2)
	require.Len(t, result[0].Tokens, 1)
	assert.Equal(t, "prod-key", result[0].Tokens[0].TokenName)
	// dev-key 不含 "prod"，被排除
	require.Len(t, result[1].Tokens, 1)
	assert.Equal(t, "prod-key", result[1].Tokens[0].TokenName)
}

func TestListConversationUsersPaginationTotal(t *testing.T) {
	setupConversationUsersTestDB(t)
	names := []string{"u1", "u2", "u3"}
	for i, id := range []int{2, 3, 4} {
		require.NoError(t, DB.Create(newTestUser(id, names[i], common.UserStatusEnabled)).Error)
	}

	// page=1, pageSize=2：返回 2 条，total=3
	page1, total, err := ListConversationUsers(&ConversationUserQueryParams{}, 1, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total)
	require.Len(t, page1, 2)

	// page=2, pageSize=2：返回剩余 1 条，total 仍为 3
	page2, total2, err := ListConversationUsers(&ConversationUserQueryParams{}, 2, 2)
	require.NoError(t, err)
	assert.Equal(t, int64(3), total2)
	require.Len(t, page2, 1)
}

func TestListConversationUsersPageNormalization(t *testing.T) {
	setupConversationUsersTestDB(t)
	require.NoError(t, DB.Create(newTestUser(2, "user1", common.UserStatusEnabled)).Error)

	// page < 1 应归一为 1，仍能取到数据而非空页
	result, total, err := ListConversationUsers(&ConversationUserQueryParams{}, 0, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, result, 1)
	assert.Equal(t, 2, result[0].UserID)
}

func TestListConversationUsersEmptyResult(t *testing.T) {
	setupConversationUsersTestDB(t)
	// 无任何用户：返回空 summary 切片与 total=0
	result, total, err := ListConversationUsers(&ConversationUserQueryParams{}, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(0), total)
	assert.Len(t, result, 0)
}

func TestListConversationUsersNilParamsSafe(t *testing.T) {
	setupConversationUsersTestDB(t)
	require.NoError(t, DB.Create(newTestUser(2, "user1", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(10, 2, "my-token", common.UserStatusEnabled)).Error)

	// params 为 nil 不应 panic，等同于无过滤
	result, total, err := ListConversationUsers(nil, 1, 10)
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, result, 1)
}

func TestListConversationUsersHonorsUserOrdering(t *testing.T) {
	setupConversationUsersTestDB(t)
	// 倒序插入，结果仍应按 user_id 升序
	require.NoError(t, DB.Create(newTestUser(5, "zoe", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestUser(2, "amy", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(30, 5, "z-key", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(10, 2, "a-key", common.UserStatusEnabled)).Error)
	require.NoError(t, DB.Create(newTestToken(11, 2, "b-key", common.UserStatusEnabled)).Error)

	result, _, err := ListConversationUsers(&ConversationUserQueryParams{}, 1, 10)
	require.NoError(t, err)
	require.Len(t, result, 2)
	// items 按 user_id 升序
	assert.Equal(t, 2, result[0].UserID)
	assert.Equal(t, 5, result[1].UserID)
	// 每用户 tokens 按 token_id 升序
	require.Len(t, result[0].Tokens, 2)
	assert.Equal(t, 10, result[0].Tokens[0].TokenID)
	assert.Equal(t, 11, result[0].Tokens[1].TokenID)
}
