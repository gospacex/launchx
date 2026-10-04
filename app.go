package launchx

import "sync"

// App 暴露基础设施只读句柄。业务在 OnSetup 内装配自己的分层，
// launchx 不对目录结构做任何约定。
type App struct {
	configPath string
	mu         sync.Mutex
	setups     []func(*App) error
	stops      []func() error
	closed     bool
}

// OnSetup 注册装配回调，Bootstrap 后、Run 前调用。
func (a *App) OnSetup(fn func(*App) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setups = append(a.setups, fn)
}

// OnStop 注册退出回调，Run 收尾时按注册逆序执行（早于内置组件 Stop）。
func (a *App) OnStop(fn func() error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stops = append(a.stops, fn)
}

// Run 启动服务并托管退出。
func (a *App) Run() error { return nil }

// Close 幂等执行反序退出链。
func (a *App) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	return nil
}
