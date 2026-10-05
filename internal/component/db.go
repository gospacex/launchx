package component

import (
	dbxconfig "github.com/gospacex/dbx/config"
	"github.com/gospacex/dbx/orm"
)

func init() { Register("db", buildDB) }

// buildDB 构造数据库组件（yaml 段：xmiddleware.sqlx.databases.<service>）。
//
// trace.on.db 只决定入不入链：true 走 orm.Open（带 OTel 回调，产 db.query 等
// span 且携带完整 SQL 文本），false 走 orm.OpenNoTrace。两者都会**构造**连接池——
// 把开关当构造开关是历史 bug，见包注释红线 2。
func buildDB(env *Env) error {
	if pickSection(env.Cfg, "xmiddleware.sqlx", "xmiddleware.dbx") == "" {
		return nil // presence-based：未配置即跳过
	}
	cfg, _, err := dbxconfig.LoadMySQLFromConfigx(env.Cfg, env.ServiceName)
	if err != nil {
		return degrade(env, "db", ConsequenceDB, err)
	}
	var db any
	if traceOn(env, "db") {
		db, err = orm.Open(cfg)
	} else {
		db, err = orm.OpenNoTrace(cfg)
	}
	if err != nil {
		return degrade(env, "db", ConsequenceDB, err)
	}
	env.DB = db
	return nil
}
