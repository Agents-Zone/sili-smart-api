package model

import (
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// 外部 DSN 必须指向专用测试库；默认只执行内存 SQLite。
func TestChannelDeletionReturnsCommittedIDs(t *testing.T) {
	for _, name := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(name, func(t *testing.T) {
			var dialect gorm.Dialector
			switch name {
			case "sqlite":
				dialect = sqlite.Open(":memory:")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("未配置专用 MySQL 测试库")
				}
				dialect = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("未配置专用 PostgreSQL 测试库")
				}
				dialect = postgres.Open(dsn)
			}
			db, err := gorm.Open(dialect, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			previousDB, previousType := DB, common.MainDatabaseType()
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(name))
			t.Cleanup(func() { DB = previousDB; common.SetMainDatabaseType(previousType) })
			require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
			var ids []int
			for _, status := range []int{common.ChannelStatusEnabled, common.ChannelStatusManuallyDisabled, common.ChannelStatusAutoDisabled} {
				channel := Channel{Key: "test", Status: status, Models: "gpt-4", Group: "default"}
				require.NoError(t, db.Create(&channel).Error)
				ids = append(ids, channel.Id)
				require.NoError(t, db.Create(&Ability{ChannelId: channel.Id, Group: "default", Model: "gpt-4", Enabled: true}).Error)
			}
			t.Cleanup(func() {
				require.NoError(t, db.Where("channel_id IN ?", ids).Delete(&Ability{}).Error)
				require.NoError(t, db.Where("id IN ?", ids).Delete(&Channel{}).Error)
			})
			deleted, err := DeleteDisabledChannel()
			require.NoError(t, err)
			assert.ElementsMatch(t, ids[1:], deleted, "仅返回事务实际删除的禁用渠道")
			var remaining []Channel
			require.NoError(t, db.Where("id IN ?", ids).Find(&remaining).Error)
			require.Len(t, remaining, 1)
			assert.Equal(t, ids[0], remaining[0].Id)
			deleted, err = BatchDeleteChannels([]int{ids[0], ids[0], ids[1]})
			require.NoError(t, err)
			assert.Equal(t, []int{ids[0]}, deleted, "重复 ID 和已删除 ID 不进入清理目标")
			var abilities int64
			require.NoError(t, db.Model(&Ability{}).Where("channel_id IN ?", ids).Count(&abilities).Error)
			assert.Zero(t, abilities)
		})
	}
}
