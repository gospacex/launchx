package pipeline

import (
	"errors"
	"fmt"
)

// Shutdown 按给定顺序执行反序退出链。
//
// 与 Run 的差别是**刻意**的：启动链任一步失败即中止（地基失败不降级）；
// 退出链相反——任一步失败都必须继续往下走，否则会漏掉 tracing.shutdown
// （冲刷最后一批 span）与 logger.close（丢缓冲日志）。错误不吞，聚合返回。
//
// 幂等：同一 Env 上第二次调用直接返回 nil，事件序列不重复记录。
func Shutdown(e *Env, steps []Step) error {
	if e == nil {
		return fmt.Errorf("pipeline: env is nil")
	}
	if e.Events == nil {
		e.Events = NewEvents()
	}
	if e.shutdownDone {
		return nil
	}
	e.shutdownDone = true

	var errs []error
	for _, s := range steps {
		e.Events.Record(s.Name)
		if s.Fn == nil {
			errs = append(errs, fmt.Errorf("pipeline step %q: nil func", s.Name))
			continue
		}
		if err := s.Fn(e); err != nil {
			errs = append(errs, fmt.Errorf("pipeline step %q: %w", s.Name, err))
		}
	}
	return errors.Join(errs...)
}
