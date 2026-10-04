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

replace (
	github.com/gospacex/cachex => ../cachex
	github.com/gospacex/cachex/redis => ../cachex/redis
	github.com/gospacex/configx => ../configx
	github.com/gospacex/coordx => ../coordx
	github.com/gospacex/coordx/consul => ../coordx/consul
	github.com/gospacex/dbx => ../dbx
	github.com/gospacex/logx => ../logx
	github.com/gospacex/mqx => ../mqx
	github.com/gospacex/mqx/kafka => ../mqx/kafka
	github.com/gospacex/otelx => ../otelx
	github.com/gospacex/traceflag => ../traceflag
	github.com/gospacex/webx => ../webx
)
