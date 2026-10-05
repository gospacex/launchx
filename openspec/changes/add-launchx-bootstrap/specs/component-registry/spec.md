# component-registry delta

## ADDED Requirements

### Requirement: 可选组件注册表

系统 SHALL 提供组件注册表（`component.Register(name, Builder)`）并在骨架第 5 步统一挂载，首个版本内置 `db`（mysql/gorm）、`cache`、`mq`、`registry`（consul）四个组件；组件构造结果存入 `App` 对应字段并暴露给 Setup。

各组件 MUST 经对应的 hubx `*x` SDK 构造 —— `db` → `dbx/orm`、`cache` → `cachex`（驱动 `cachex/redis`）、`mq` → `mqx`（驱动 `mqx/kafka`）、`registry` → `coordx/consul`；launchx MUST NOT 直接 require gorm / go-redis / kafka-go / consul-api 等三方客户端。

#### Scenario: 组件挂载可用

- **WHEN** yaml 声明 db/cache/mq 且初始化成功
- **THEN** `App.DB`/`App.Cache`/`App.MQ` 非 nil 且 Setup 中可用

### Requirement: Trace.On 门控只控入链

每个组件 SHALL 受 `trace.on.<name>` 开关门控，该开关 MUST 只决定是否注册 OTel 回调（是否入链），MUST NOT 决定组件是否构造；开关关闭时组件仍正常构造与工作。

开关判定 SHALL 经 `github.com/gospacex/traceflag` 执行（语义：实例级 AND 组件级 AND 全局总闸，三者皆开才埋点）；launchx 只负责把 yaml `trace.on.<name>` 映射为 traceflag 的组件级开关注册，MUST NOT 自造布尔判定逻辑。

#### Scenario: 验收案例 D1 — mq 开关关闭不中断投递

- **WHEN** `trace.on.mq: false` 且业务正常发送事件
- **THEN** producer 正常构造并投递消息（仅不产生 span），outbox 不出现无限积压

### Requirement: strict 策略裁决组件失败

组件初始化失败时，系统 SHALL 按 `strict.<name>` 裁决：缺省（false）→ 对应 `App` 字段置 nil 降级态并记录含组件名与后果的 `logger.Warn`，启动继续；`true` → `Bootstrap` 返回带 `%w` 包装的 error。地基组件（配置/telemetry/logx）失败 MUST 始终即退，不受 strict 影响。

#### Scenario: 验收案例 D2 — cache 失败默认降级

- **WHEN** cache 初始化失败且未设置 `strict.cache: true`
- **THEN** 启动继续、`App.Cache` 为 nil、日志含降级后果说明，依赖方判 nil 后 fail-open（如限流）

#### Scenario: strict 置真失败即退

- **WHEN** db 初始化失败且 `strict.db: true`
- **THEN** `Bootstrap` 返回含 `init db` 上下文的 error

### Requirement: registry 失败带病运行

consul 注册失败时，除 `strict.registry: true` 外 SHALL 记录 Warn 并继续运行（未注册态），退出时对已注册状态执行注销。

#### Scenario: 无 consul 启动

- **WHEN** consul 不可达且 `strict.registry` 未置 true
- **THEN** 服务正常启动并 Warn，本地调用链路可用
