module github.com/gospacex/launchx

go 1.27.1

require (
	github.com/gospacex/cachex v0.0.0
	github.com/gospacex/cachex/redis v0.0.0
	github.com/gospacex/configx v0.0.0
	github.com/gospacex/coordx v0.0.0
	github.com/gospacex/coordx/consul v0.0.0
	github.com/gospacex/dbx v0.0.0
	github.com/gospacex/logx v0.0.0
	github.com/gospacex/mqx v0.0.0
	github.com/gospacex/mqx/kafka v0.0.0
	github.com/gospacex/otelx v0.0.0
	github.com/gospacex/traceflag v0.0.0
	github.com/gospacex/webx v0.0.0
)

// 以下 7 条不由 launchx 直接 import，仅在依赖模块（cachex / coordx / mqx / dbx）
// 的 require 中以 v0.0.0-00010101000000-000000000000 伪版本出现。依赖模块自身的
// replace 会被主模块忽略，故必须在此补齐，否则 MVS 图无法闭环、解析会去访问
// 不存在的 github 仓库。
replace (
	github.com/gospacex/cachex => ../cachex
	github.com/gospacex/cachex/memcached => ../cachex/memcached
	github.com/gospacex/cachex/redis => ../cachex/redis
	github.com/gospacex/configx => ../configx
	github.com/gospacex/coordx => ../coordx
	github.com/gospacex/coordx/consul => ../coordx/consul
	github.com/gospacex/coordx/etcd => ../coordx/etcd
	github.com/gospacex/dbx => ../dbx
	github.com/gospacex/hubx => ../hubx
	github.com/gospacex/logx => ../logx
	github.com/gospacex/mqx => ../mqx
	github.com/gospacex/mqx/kafka => ../mqx/kafka
	github.com/gospacex/mqx/nsq => ../mqx/nsq
	github.com/gospacex/mqx/pulsar => ../mqx/pulsar
	github.com/gospacex/mqx/rabbitmq => ../mqx/rabbitmq
	github.com/gospacex/mqx/rocketmq => ../mqx/rocketmq
	github.com/gospacex/otelx => ../otelx
	github.com/gospacex/traceflag => ../traceflag
	github.com/gospacex/webx => ../webx
)
