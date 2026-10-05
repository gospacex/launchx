package component

import (
	"fmt"

	mongodbx "github.com/gospacex/mongodbx" // 包名为 docx，此处显式改名以对齐模块名
)

func init() { Register("mongodb", buildMongo) }

// mongoSection 是 xmiddleware.mongodbx 段的最小映射。
type mongoSection struct {
	Instances map[string]mongodbx.Config `mapstructure:"instances"`
}

// buildMongo 构造文档数据库组件（yaml 段：xmiddleware.mongodbx）。
//
// moongodbx 的调用链开关是**全局**的（mongodbx/tracing.go，默认是关的，
// 不显式打开连一个 span 都不会产）：实例级 enable_trace 不存在，必须经
// SetTracingConfig 写入全局，由 trace.on.mongodb 裁决。
//
// 顺带接住 MapToStruct 填不进去的开关：Config 字段没有 mapstructure tag，
// enable_trace 这种蛇形键会被静默丢弃（比字段名拼错更难发现）。
func buildMongo(env *Env) error {
	key := pickSection(env.Cfg, "xmiddleware.mongodbx", "mongodbx")
	if key == "" {
		return nil
	}
	wantTrace := traceOn(env, "mongodb")
	mongodbx.SetTracingConfig(mongodbx.TracingConfig{Enabled: wantTrace})
	var sec mongoSection
	if err := env.Cfg.Unmarshal(key, &sec); err != nil {
		return degrade(env, "mongodb", ConsequenceMongo, err)
	}
	if len(sec.Instances) == 0 {
		return degrade(env, "mongodb", ConsequenceMongo, fmt.Errorf("%s.instances is empty", key))
	}
	cfgs := make([]*mongodbx.Config, 0, len(sec.Instances))
	for name := range sec.Instances {
		inst := sec.Instances[name]
		inst.Name = name
		if inst.Driver == "" {
			inst.Driver = "mongodb"
		}
		if inst.Database == "" {
			inst.Database = env.ServiceName
		}
		inst.EnableTrace = wantTrace // 实例级与主开关保持一致，避免两处开关互相打架
		cfgs = append(cfgs, &inst)
	}
	instances, err := mongodbx.New(cfgs)
	if err != nil {
		return degrade(env, "mongodb", ConsequenceMongo, err)
	}
	env.Mongo = instances
	env.AddStop("mongodb", func() error {
		for _, db := range instances {
			if db != nil {
				db.Close()
			}
		}
		return nil
	})
	return nil
}
