package configsrc

import (
	"os"

	"github.com/gospacex/configx"
)

// envConfig 包装 configx.Config，对字符串类读取施加 os.ExpandEnv。
// 展开点唯一在此，避免各处零散 ExpandEnv 导致口径漂移。
//
// 边界（已裁决保持的现状，非缺陷）：仅标量字符串读取（GetString / Get）展开；
// GetMap / Unmarshal / GetSlice 返回未展开的原值。dbx 等消费方取 DSN 走
// GetString，故 ${ENV} 生效，无需在此额外展开子树。
type envConfig struct{ configx.Config }

func (e *envConfig) GetString(key string) string {
	return os.ExpandEnv(e.Config.GetString(key))
}

func (e *envConfig) Get(key string) interface{} {
	v := e.Config.Get(key)
	if s, ok := v.(string); ok {
		return os.ExpandEnv(s)
	}
	return v
}
