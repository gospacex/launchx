# dtm-deployment Specification

## Purpose
TBD - created by archiving change add-micro-dtm-saga-tcc. Update Purpose after archive.
## Requirements
### Requirement: DTM server SHALL 以官方 docker 形态部署并用独立 dtm_db

DTM server SHALL 以官方镜像 + 官方环境变量形态（HTTP :36789 / gRPC :36790，
`STORE_DRIVER=mysql` + `STORE_HOST/PORT/USER/PASSWORD/DB`）追加进现有中间件底座
docker-compose；存储库 SHALL 为独立 `dtm_db`，业务四库不存放
dtm 内部表。官方文档的 docker run 形态 SHALL 以等价的 compose service 落成，
行为口径一致。

> **2026-10-14 实测更正**：原规格写的是「dtm 自建 trans_global / trans_branch_op
> 等事务表」，**这条是错的，已更正**。实测（`yedf/dtm:latest` v1.19.0，指向一个
> 全新空库）：dtm **不会**创建任何表，而是每隔 3 秒 recover 一次
> `Table 'xxx.trans_global' doesn't exist` 后继续存活，端口照常监听、HTTP 面照常
> 应答 `newGid` —— 20 分钟累计 328 次 panic，期间一条事务都登记不进去。
> 因此表必须由 compose 的 `dtm-db-init` 一次性容器从
> `dockers/dtm-sql/dtmsvr.mysql.sql`（幂等 DDL）灌入，且 dtm SHALL 依赖它
> `service_completed_successfully` 后再启动。
> 连带更正的健康判据：**容器 healthy / 端口通都不足以证明 dtm 可用**，唯一可信
> 的判据是 `docker logs dtm | grep -c "recovered panic"` 为 0。

#### Scenario: dtm 起服即可用

- **WHEN** `docker compose up -d` 起底座（含 dtm 服务）
- **THEN** :36790 gRPC 可达、HTTP :36789 应答 `newGid`，`dtm_db` 内有
  `trans_global` / `trans_branch_op` / `kv` 三张表（由 `dtm-db-init` 灌入、
  **不是** dtm 自建），`recovered panic` 计数为 0；业务四库 dtm 表数为零

#### Scenario: dtm server 重启自动续推进未完成事务（原 §3 的运行时验证）

- **WHEN** dtm 容器被 `docker kill -s KILL` 后 `compose up -d dtm` 重启
- **THEN** 处于 `submitted` 的在途事务 `update_time` 立刻被推进（`next_cron_time`
  刷新）；冻结期间（dtm 不在）两次查询的 `update_time` 逐字相同

### Requirement: 分支回调地址 MUST NOT 用回环地址

bff 配给 dtm 的分支回调地址（`dtm.branches.<domain>`）MUST NOT 写成
`127.0.0.1:904x`：dtm server 一旦以容器形态运行，它回调时的 `127.0.0.1` 是
**容器自己**，不是宿主上的域服进程。该地址 SHALL 写成
`host.docker.internal:904x`（容器内由 docker 注入 DNS 指向宿主；宿主侧它自身也
解析到 `127.0.0.1`，故进程式 dtm 同样适用 —— 两种形态共用一份配置）。

#### Scenario: 回环地址会导致分支永久悬停（2026-10-14 实测，反向探针）

- **WHEN** dtm 以容器运行、bff 登记的分支 URL 为 `127.0.0.1:9043/dtmbranch.stock.StockBusi/Deduct`
- **THEN** `trans_global` 有 `submitted` 记录且 `trans_branch_op` 八个分支全停在
  `status=prepared`、`finish_time` 为 NULL；dtm 日志反复出现
  `dial tcp 127.0.0.1:9043: connect: connection refused`，事务永不见终态
- **WHEN** 换成 `host.docker.internal:9043` 的同一条事务
- **THEN** 立刻拿到来自宿主进程的应用层 gRPC 应答（`code = Internal`，
  因手工 HTTP 提交的载荷非 protobuf wire format），证明回调链路已通

> 这条与 Kafka 的 advertised `127.0.0.1:9092` 是同一个坑的两种长相：**容器里的
> `127.0.0.1` 是容器自己**。判据别放在「端口通不通」上 —— 此处 bff 收到 gid、
> HTTP 面正常应答、进程也健康，一切都绿，只有 `trans_branch_op.finish_time`
> 揭得穿。

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

