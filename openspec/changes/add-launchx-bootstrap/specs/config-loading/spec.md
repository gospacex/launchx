# config-loading delta

## ADDED Requirements

### Requirement: 可插拔配置源

系统 SHALL 以注册表形式提供配置源加载器（`configsrc.Register(name, Loader)`），yaml `configcenter.type` 缺省时使用 `local`，声明后按名选择对应远端源；首个版本 MUST 内置 `local` 与 `nacos` 两个加载器。

#### Scenario: 缺省走 local

- **WHEN** yaml 未声明 `configcenter` 段
- **THEN** `Bootstrap` 仅从本地 `path` 指定文件加载配置，不发起任何远端请求

#### Scenario: 声明才启用远端

- **WHEN** yaml 声明 `configcenter.type: nacos`
- **THEN** 启用 nacos 加载器并按合并规则产出最终配置

### Requirement: local 加载与环境变量展开

`local` 加载器 SHALL 经 configx 读取本地 yaml，并在使用前对 DSN 等字段中的 `${ENV}` 占位执行 `os.ExpandEnv` 展开（保持 micro_apps 既有语义：configx 本身不做 env 展开）。

#### Scenario: DSN 密码占位展开

- **WHEN** 本地 yaml 中 DSN 含 `${HUBX_DB_PW}` 占位且环境变量已设置
- **THEN** 组件拿到的 DSN 为展开后的真实值

### Requirement: nacos 合并三裁决规则

`nacos` 加载器 SHALL 实现与 micro_apps `LoadMerged` 一致的三裁决规则：本地 config.yaml 兜底、远程 dataId（`hubx.<service>` 等）key 级覆盖本地、Nacos 地址由 env（`HUBX_NACOS_SERVER`）覆盖 yaml；dataId 为空时 MUST 纯本地运行。

#### Scenario: 验收案例 B1 — key 级裁决

- **WHEN** 本地与远程同键不同值
- **THEN** 远程值生效，未在远程出现的键保留本地值

#### Scenario: 远端不可达按地基失败

- **WHEN** `configcenter.type: nacos` 已声明且 Nacos 不可达
- **THEN** `Bootstrap` 返回 error（地基失败不降级），错误信息含 dataId 与地址上下文
