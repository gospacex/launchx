# dtm-saga-ordering delta

## ADDED Requirements

### Requirement: 复合下单 SHALL 以 DTM Saga 提交并替换现有手动编排

`examples/micro/bff` 的复合下单（`POST /api/order`）SHALL 以 DTM **SAGA** 事务
提交：bff 经 `dtm-labs/client` 的 `dtmgrpc` API 组四分支 Saga，DTM server 地址取自
config（`xmiddleware.dtm.grpc_addr`）。分支序 SHALL 为
`stock.Deduct → order.CreateOrder（含 outbox 行）→ product.销量+1 → points.Debit`，
每分支配逆序补偿（`stock.Release` / 订单作废 / 销量-1 / `points.Refund`）。
现有手动 `demo-stock.ReleaseStock` 补偿代码 SHALL 退役删除。

提交前 SHALL 先做 `product.GetProduct` 普通预检（同一 trace 的普通 RPC 读，
非事务分支）；预检失败 MUST NOT 提交事务并返回 4xx。

#### Scenario: 四分支全绿落库

- **WHEN** `POST /api/order` 携带有效凭据与库存充足的商品提交
- **THEN** dtm 推进全部四分支成功：`stock_db` 库存扣减且流水落行、`order_db`
  orders/`order_items`/outbox 三表同事务落行、`product_db` 销量 +1、
  `points_db` 扣点且流水落行；HTTP 返回含全局事务 ID（gid）

#### Scenario: 业务失败自动逆序补偿

- **WHEN** 任一分支持行 fail inject（载荷 `fail_inject` 字段命中该分支）返回
  业务失败（`dtmgrpc` 语义，不重试）
- **THEN** dtm 自动按已成功分支逆序补偿：points 退回 → 销量 -1 → 订单作废 →
  库存释放；最终四库回到提交前基线；HTTP 返回业务失败码

#### Scenario: 预检失败不进事务

- **WHEN** `POST /api/order` 引用的商品在 `product.GetProduct` 预检阶段不可得
- **THEN** 返回 4xx，`dtm_db` 无新全局事务行、四库均无写入

### Requirement: 分支失败语义 SHALL 显式区分业务失败与基础失败

各域 service 层新增的 Saga 分支方法 SHALL 显式返回两类错误：
**业务失败**（业务规则不满足，如库存不足/积分不足，映射为不重试的
`ResultFailure` 语义）与**基础失败**（网络/gRPC error/超时，dtm 按重试策略自动重试）。
分支面 MUST NOT 把业务失败包装成可重试错误，MUST NOT 把基础失败包装成业务失败。

#### Scenario: 积分不足走业务失败不重试

- **WHEN** `points.Debit` 分支发现余额不足
- **THEN** 该分支返回业务失败且 dtm 立即进入补偿（`trans_branch_op` 记录
  非重试终态），不动 points_db 余额

#### Scenario: 基础失败由 dtm 重试直至成功或补偿

- **WHEN** `order.CreateOrder` 分支的 gRPC 回调因网络错误暂时不可达
- **THEN** dtm 按其重试策略重复回调该分支（barrier 保证幂等），不触发补偿；
  分支最终成功或转为补偿路径
