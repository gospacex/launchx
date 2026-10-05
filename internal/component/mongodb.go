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
// mongodbx 的调用链开关是**全局**的（mongodbx/tracing.go），没有实例级
// enable_trace；因此这里只用 presence + trace.on.mongodb 决定是否构造后的入链，
// 不往 Config 上写不存在的字段。
func buildMongo(env *Env) error {
	key := pickSection(env.Cfg, "xmiddleware.mongodbx", "mongodbx")
	if key == "" {
		return nil
	}
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
