# dtm-deployment Specification

## Purpose
TBD - created by archiving change add-micro-dtm-saga-tcc. Update Purpose after archive.
## Requirements
### Requirement: DTM server SHALL 以官方 docker 形态部署并用独立 dtm_db

DTM server SHALL 以官方镜像 + 官方环境变量形态（HTTP :36789 / gRPC :36790，
`STORE_DRIVER=mysql` + `STORE_HOST/PORT/USER/PASSWORD/DB`）追加进现有中间件底座
docker-compose；存储库 SHALL 为独立 `dtm_db`（dtm 自建事务表），业务四库不存放
dtm 内部表，密码复用 `HUBX_DB_PW`。官方文档的 docker run 形态 SHALL 以等价的
compose service 落成，行为口径一致。

#### Scenario: dtm 起服即可用

- **WHEN** `docker compose up -d` 起底座（含 dtm 服务）
- **THEN** :36790 gRPC 可达，dtm 在 `${HUBX_DB_PW}` 凭据下对 `dtm_db` 自建
  `trans_global` / `trans_branch_op` 等事务表；业务四库 dtm 表数为零

### Requirement: 新地址 SHALL 全部落配置且 dtm 不可达为带病运行

bff 与四域服 SHALL 把本变更全部新增地址写入各自 `config.yaml`，业务 Go 代码
MUST NOT 出现硬编码地址：bff SHALL 配 `xmiddleware.dtm.grpc_addr`
（缺省 `127.0.0.1:36790`），四域服 SHALL 配 `dtm.branch.listen_addr`
（缺省 :9041-9044）。dtm server 不可达时 SHALL 按 `strict.dtm: false`
带病运行：bff 提交返回 5xx +
Warn（带 `consequence` 与 `hint`），进程不退，MUST NOT 回落到已退役的手动链。

#### Scenario: 地址零硬编码

- **WHEN** 对 `examples/micro` 新增业务源码跑「无硬编码回环地址」守卫测试
- **THEN** 全绿；dtm 地址与分支面端口只出现在 `config.yaml`

#### Scenario: dtm 挂掉不拖垮进程

- **WHEN** 停止 dtm 容器后向 bff 发一次下单请求
- **THEN** 返回 5xx 与一条带 `consequence`/`hint` 的 Warn 日志；bff 进程存活，
  后续请求持续可服务（不 crash）

### Requirement: dtm server 重启 SHALL 自动续推进未完成事务

全局事务状态持久于 `dtm_db`。dtm server kill 后重启 SHALL 自动继续推进
处于进行中的全局事务（未完成分支重试 / 补偿），MUST NOT 丢失事务或悬停。

#### Scenario: kill 后续推进

- **WHEN** 在一条 Saga 全局事务推进到第二分支时 kill dtm 进程再重启
- **THEN** dtm 按事务表恢复并继续执行该 gid 既定路径（成功则完成、业务失败则补偿），
  `dtm_e2e.sh` 的重启实验判据为绿

