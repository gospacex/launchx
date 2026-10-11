# launchx 全仓库 Code Review

- 日期：2026-10-10
- 范围：全仓库通读 —— 框架层（根包 `*.go` + `internal/**`）、四组示例（`monolith` / `micro` / `mall` / `mesh`）、非 Go 面（`scripts/**`、`deploy/**`、`.github/workflows/**`、`docker/**`）与文档口径
- 方法：框架层由本人逐行精读；四组示例与部署面由四个并行评审员全量通读后汇总，本人对其中「已知易错点」逐条复核（部分结论被推翻，见各章「核查结论」）
- 分级：`[必须修复]` 不修不能合｜`[建议修改]` 本次或下个迭代｜`[仅供参考]` 不改也行｜`[问题]` 需作者解释意图
- 判定：**Request changes**

---

## 0. 总体判定

**一句话**：这是一份「工程自觉明显高于平均水准」的仓库——框架层的退出链有界、降级带后果文案、判据脚本自带反向断言、注释里写满了「为什么」；但**四组示例在抄同一批业务级缺陷**（幂等、越权、缓存穿透、计数竞态），且**几处用于自证的判据本身是失效的**（正则恒不命中、BRE 交替失效、脚本吞错），导致门禁绿得没有依据。

距离「企业级可上线参考实现」差的主要是三类，与代码风格无关：

1. **幂等与分布式一致性**（下单无幂等键、库存预占无唯一键、释放/确认无锁、异步链路无消费端）；
2. **授权缺失**（鉴权只做「是不是有效身份」，不做「能看谁的数据」）；
3. **判据失效**（自证的 grep/正则在当前工具链下恒不命中，把违规判成合规）。

---

## 1. 跨案例同源缺陷（最高优先级，先修这批）

以下缺陷在 2~4 组示例里**各自独立复制了一份**。它们不是巧合，是 `srv-order/stock/product/points` 四域「复制粘贴式分层」的直接后果。**修一处要同步四处**，否则下一轮评审还会再出现一遍。

### [必须修复] 库存预占没有幂等约束：无唯一键 + 事务内不查重

- `examples/micro/srv-stock/internal/repository/stock.go:206`（`ReserveTx`）
- `examples/mall/srv-stock/internal/repository/stock.go:206`（同构）

现象：`ReserveTx` 只做 `WHERE available >= ?` 条件扣减 + 无条件插入预占流水；表上 `stock_reservations` 只有普通索引 `idx_sr_order_product`，**没有** `(order_no, product_id)` 唯一键。  
影响：bff 重试 / 网关重发一次就多冻结一份库存；而 `ReleaseTx` 用 `Order("id DESC").Take` 只认最新一条，旧流水永久停在 `status=1` ⇒ 库存永久泄漏，`stocks.reserved` 与流水聚合值持续漂移，`auditDrift` 会一直告警却修不回来。  
建议：① 加 `UNIQUE KEY uk_sr_order_product (order_no, product_id)`；② `ReserveTx` 事务内先查同键流水，命中即按幂等成功返回（回已有 `reservation_id` 与当前 `available`，不再扣）；③ 同步改掉 bff 那句「同一单重放不会重复扣减」的注释（它现在是假的）。

### [必须修复] 下单链路没有跨请求幂等键：四域各用各的关联号

- `examples/micro/bff/internal/handler/mall.go:200`（`newOrderNo`）、`:232`（`CreateOrder` 不传 OrderNo）
- `examples/mall/srv-bff/internal/handler/mall.go:174`、`:206`、`:316`

现象：bff 现生成一个 `orderNo`，只用于库存预占与积分入账；`CreateOrderReq` 里**没有** order_no 字段，订单域自己另起一个号。于是同一次下单在三处留下三个互不相干的关联号。  
影响：跨库对账无法 join；重放一次 = 新号 + 新单 + 新预占，而积分侧会被 `uk_ledger_order_type` 幂等掉 ⇒ 「下了两单、只送一次积分」的对账差异。订单表现成的 `uk_order_no` 完全没被用作幂等事实来源。  
建议：给 `CreateOrderReq` 加 `order_no`（或 `idempotency_key`），bff 透传，订单域事务内先按该键查重；**同一条**：把 `:316` 那句「同一单重放不会重复扣减」的注释改成如实描述。

### [必须修复] 鉴权只认证不授权：请求里的 user_id 无人校验归属（IDOR）

- `examples/monolith/internal/middleware/auth.go:76`（`c.Set("user_id", …)` 后全仓无 `c.Get("user_id")` 读取点）
- `examples/micro/bff/internal/handler/mall.go:144`（`GetAccount` 用路径参数，同文件其余三个 handler 用的是 `userIDOf`）
- `examples/mall/srv-bff/app/setup.go:126-132`（bff 与 gateway **完全没有** `engine.Use` 挂鉴权）
- `examples/micro/pkg/auth/gate.go:27-51`（`Authenticate` 只看身份自身字段，不与请求里的 `operator_id` 比对）

现象：闸门通过后 handler 完全信任 req/query 里的 `user_id`。micro 的 `userIDOf` 注释（`mall.go:370-374`）自己写着「否则客户端只要在 URL 上换个 user_id 就能读别人的订单，鉴权形同虚设」——唯独 `GetAccount` 漏了。  
影响：持有任一有效令牌即可枚举他人订单/积分账户；mall 组更彻底——入口层根本没门。作为「可上线参考实现」，这是会被直接照搬出去的越权面。  
建议：① 四组统一用 `userIDOf(ctx, c, …)` 取主体，并在其中加「身份存在且与请求参数不一致 → 403」的兜底；② mall 的 bff/gateway 在路由注册**之前**挂鉴权中间件；③ 至少在 README 用醒目段落写明「非 /secure 路径为验收脚本入口，无鉴权，不得照搬至生产」。

### [建议修改] 限流/熔断计数用 `Incr` 后再 `Set` 补 TTL，并发下覆盖写

- `examples/monolith/internal/ratelimit/counter.go:48-59`
- `examples/micro/srv-stock/internal/middleware/ratelimit.go:49-55`、`internal/service/stock.go:372-379`
- `examples/mall`（同构）

现象：`kvx.KVStore` 没有 `Expire`，当前写法是 `n, _ := Incr(key); if n == 1 { Set(key, "1", ttl) }`。A 拿到 `n==1` 后、执行 `Set` 之前，B 完成 `Incr`（值变 2），A 的 `Set` 把计数打回 1。  
影响：窗口开头并发突刺少计 ⇒ 限流偏松、熔断触发偏晚。而 `Set` 的错误被 `_ =` 丢弃，线上无任何痕迹。  
建议：给 kvx 补 `Expire(ctx, key, ttl)`（Redis `EXPIRE`，不改值）并改用它；落地前至少把 `_ =` 改成记 Warn + span 属性，并把注释里「此时值必然是 1」这句改成如实的启发式描述。

### [建议修改] 缓存穿透：查询不存在的 ID 不回填空值

- `examples/micro/srv-order/internal/service/order.go:206-212`（product/stock/points 同构）
- `examples/mall/srv-product/internal/service/product.go:180`（四域同构）

现象：`repo.GetX` 返回 `ErrNotFound` 时直接 return，不写缓存；下次同样的 ID 仍完整走一遍 DB（订单/商品还是 JOIN）。  
影响：刷一个不存在 ID 的字典就能把 100% 流量打到数据库，缓存形同虚设。  
建议：回填短 TTL（30-60s）的空值哨兵，命中直接返回对应 NotFound 码，并记 `cache.negative=true` 属性便于观测。

### [建议修改] 四域之间的死复制没有「不许漂移」的机制

- `examples/micro/srv-{order,points,product,stock}/app/setup.go`（各约 160 行，差异只有服务名与阈值）
- `settle()` 四份逐字相同、`rejected()` 四份逐字相同、`withXLock` / `evTraceID` / `invalidate` 结构完全一致
- `examples/mall` 六进程同构：`main.go` / `ratelimit.go` / `settle` / `begin+quietNotFound` / `rateLimited+subjectOf` 各四份

现象：`examples/pkg/outbox` 已经树立了「公共骨架上提」的先例，但这些真正零域差异的部分仍在四域各抄一份。`rejected` 的注释自己写着「两处用各自的字符串常量，改名的那天就会静默退化」——担忧写出来了，却没有机制兜底。  
建议：把 `settle` / `rejected` / `ticketTTL` / `evTraceID` / outbox 装配收进 `examples/pkg`；在改之前至少加一条测试钉住「四份实现必须一致」。

---

## 2. 框架层（根包 + internal）

### [建议修改] `pipeline.Shutdown` 无人调用，与 `App.Close` 形成两份退出链

位置：`internal/pipeline/shutdown.go:15`；实际生效的实现在 `app.go:805`（`App.Close`）  
现象：`Shutdown` 定义了「反序退出链 + 幂等 + 错误聚合」，但全仓无调用点；`App.Close` 另起一套显式七步序列，且没走 `Shutdown` 的幂等闸门（用的是自己的 `a.closed`）。  
影响：两处都声称自己是退出链。将来有人修「退出链超时不进错误聚合」这类语义，改了 `pipeline.Shutdown` 会发现行为毫无变化；反之亦然。  
建议：二选一——把 `App.Close` 的七步做成 `[]Step` 交给 `pipeline.Shutdown` 执行（幂等与聚合都在一处），或删掉 `pipeline.Shutdown` 并在 `app.go` 注明「退出链唯一实现在此」。（`component.Mount` 同样是零调用的导出 API，若定位是「给扩展方保留的装配入口」请在包注释写明，否则按死代码处理。）

### [建议修改] `startMetricsListener` 的注释声称「晚于服务端关闭」，实际必然早于

位置：`app.go:766-768`（注释） vs `app.go:820-835`（`Close` 的实际顺序）  
现象：注释写「监听挂在 OnStop 上……反序退出时也就晚于服务端关闭，停机过程本身的指标不会被提前掐断」。但 `Close` 的顺序是「业务 OnStop（逆序）→ server.stop」，`metrics` 这条是最后注册的 OnStop，逆序执行时**最先**跑，必然早于 `server.stop`。  
影响：停机期间的指标采集口已关；更要紧的是注释给了相反的安全感，后面的人不会去查。  
建议：要么把 metrics 的关闭挪到 `server.stop` 之后（单独一步，与 Close 第 6 步的 `Shutdown` 合并），要么把注释改成如实描述。

### [建议修改] `Bootstrap` 吞掉退出链的错误

位置：`launchx.go:19`（`_ = a.Close()`）  
现象：地基失败时走完整反序退出链，但 `Close` 的返回值被丢弃——tracing 冲刷失败、组件 stop 失败、注销失败一概看不到。  
影响：启动失败时最需要知道「已初始化的资源收干净没有」，而这正是被丢掉的那部分信息。与同文件注释「已初始化的资源按反序退出链回收后再返回错误」不完全一致——回收了，但回收结果没说。  
建议：`if err := a.Close(); err != nil { return nil, errors.Join(err, fmt.Errorf("launchx: bootstrap: %w", err)) }`，或用 `a.Logger.Warn` 逐条落日志（启动失败时日志器可能已就绪）。

### [建议修改] 采样策略硬编码 `always_on`，且没有任何配置项

位置：`internal/telemetry/telemetry.go:49`（`Sampler: otelx.SamplerConfig{Type: "always_on"}`）  
现象：`Init` 只收 `serviceName / endpoint / enabled` 三个入参，采样率既不可配也不随 `trace.enabled` 变化。  
影响：生产高 QPS 下 100% 采样是实打实的成本与 exporter 背压风险；而这是**脚手架的默认值**，所有下游业务默认继承。库层不提供出口，业务只能绕过 `telemetry.Init` 自己去 `otel.SetTracerProvider`。  
建议：`Init` 增加采样配置（至少 `trace.sampler` / `trace.sampler_ratio` 两个键，缺省 `parentbased_traceidratio` + 0.01 之类），并在包注释写明「always_on 只适合本地与验收」。

### [建议修改] `selfAddr` 接受空 host，注册地址可能是 `:8080` 这种形式

位置：`app.go:656-676`  
现象：`net.SplitHostPort(":8080")` 返回 `host=""`，`selfAddr` 不判空，于是注册实例的 `Addr` 为空字符串；ID 也会拼成 `svc--8080`。  
影响：上游按注册的地址连过来，连到空主机；而日志里「service registered」照常打印，症状要到调用侧 502 才暴露。监听地址写 `:8080` 是很常见的写法。  
建议：host 为空时返回 error（`configured address has no host, use 0.0.0.0:8080 or an explicit address`），或按 `service.self.addr` 缺失时回落到本机出口 IP 并 Warn。

### [仅供参考] `Close` 里 `rec("setup.stop")` 用字面量，其余步骤用 `pipeline.Step*` 常量

位置：`app.go:821`  
现象：退出链七步里六步走常量，第一步是硬编码字符串 `"setup.stop"`。  
影响：顺序断言（`Events`）若按常量比对，这一步匹配不上；也只是少一处单一来源。  
建议：在 `pipeline` 里补 `StepSetupStop = "setup.stop"` 并统一引用。

### 已审查，未发现问题

- **退出链有界性**：`runStop` 给每一环限时（默认 5s，可配 `service.stop_timeout`），超时只 `Warn` + 记 `stop.timeout.<name>` 不进错误聚合（`app.go:909-922` 的取舍论证写得很清楚）；tracing shutdown 另有 15s 硬上限；`registryOpTimeout` 单独限时。这一块做得比多数框架扎实。
- **并发安全**：`App` 的 `setups` / `stops` / `rpcServer` / `serverStop` 全部走 `a.mu`；`setupSnapshot` 与 `Close` 里的 `stops` 都先 copy 再执行，不持锁跑业务代码。
- **依赖面红线**：库源码不 import gorm/redis/kafka/hertz/kitex，服务端句柄以 `any` + 接口断言承接（`webxStop` / `rpcStop`），并有 `dependency_surface_test.go` 与 `examples_layout_test.go` 两条守卫。
- **降级语义**：`degrade` 按 `strict.<name>` 裁决、日志带「后果」与「提示」；注册与发现的后果文案分开（`ConsequenceRegistry` vs `ConsequenceDiscovery`）；组件包三条红线写在包注释首屏。
- **`Constructed()` 的 typed nil 陷阱**：`wiring/wiring.go:49-61` 用反射判 nil 并写清了「map/slice 零值装进 any 后与 nil 恒不等」这个坑——这是真正踩过才知道的细节。

---

## 3. examples/monolith

（22 个 Go 文件 / 4k 行，26 条问题：5 必须 / 14 建议 / 7 参考）

### [必须修复] 删除订单：主子表不在同一事务，且删不存在的订单返回 200

位置：`internal/service/order.go:310-329` + `internal/repository/order.go:147-152`  
现象：先 `DeleteItems` 再 `Delete`，两条语句各自提交；且 gorm 的 `Delete` 在 `RowsAffected=0` 时 `Error=nil`，所以 `errors.Is(err, ErrNotFound)` 是永不成立的死分支。  
影响：中途失败留下「主单在、明细没了」的孤儿订单；`DELETE /orders/不存在` 返回 `200 {"deleted":true}`，README 的 `| jq .deleted # true` 在任何输入下都绿。  
建议：`repository` 增 `DeleteWithItems`，在 `db.Transaction` 内删，并用 `RowsAffected == 0` 归一化为 `ErrNotFound` 走 404。

### [必须修复] 错误响应回吐内部错误原文，与注释承诺相反

位置：`internal/handler/order.go:193-199`  
现象：注释写「响应体只带领域文案（不泄漏内部细节）」，实现是 `c.JSON(status, {"error": err.Error()})`；`statusOf` 的 default 把未知错误映射 500，此时 `err` 是驱动原文（`dial tcp 127.0.0.1:3306: connect: connection refused`、`Unknown column 'item' …`）。  
影响：内部拓扑与表结构直接回给调用方（CWE-209）；注释给出了「已脱敏」的错误安全感，比不写注释更危险。  
建议：`fail` 只输出与错误类型绑定的固定文案，内部错误统一回 `"internal error"`，详情只进日志与 span。

### [必须修复] `internal/route` 的 Router 从未实例化，`route.order.<Op>` 整层缺失

位置：`app/routes.go:27-53`（只调了 `srroute.Table()`）；实现在 `internal/route/route.go:93`、`:114`  
现象：`Router.New/Register/Handle` 与 `warned sync.Map` 零调用点，`Setup` 从未 `engine.Use(router.Handle)`。  
影响：文档描述的链路位次在真实 trace 上不存在；OTel 的 `http.route` 无人写入，根 span 只有实际路径名（`GET /orders/NO1791.../join`），`route.go:11-15` 自己论证过的「无法按路由聚合耗时与错误率」一个字都没解决；「漏登记新接口」的兜底永不触发。  
建议：`Setup` 里 `router := srroute.New("order", …)` 并在限流中间件**之前** `engine.Use(router.Handle)`；`trace_e2e_test.go` 补一条「每条 trace 必须存在 `route.order.*` 且带 `http.route`」的断言——当前 T2–T5 全没覆盖这层，所以缺失至今无人发现。

### [建议修改]（摘录，完整清单见评审员输出）

- 缓存失效晚于更新，并发读会把旧值复活进缓存（`internal/service/order.go:287-307`）——改为更新后写回新值，或加延迟双删。
- `handler.Update` 吞掉绑定错误（`:161`，`_ = c.Bind(&req)`）——与 `Create` 的显式判错口径不一致。
- 演示令牌播种失败仍注册鉴权中间件，且 1h TTL 无续期（`app/setup.go:171-177`）——与「宁可不注册，也不注册恒 401 的空壳」的自我声明相反。
- `unit_price <= 0` 被静默改成 9.9，与 `qty` 的拒绝口径不一致（`service/order.go:184-189`）。
- 订单号用纳秒时间戳（`service/order.go:365`），并发建单撞 `uk_orders_order_no` 后回滚 500。
- 用错误文本匹配决定状态码（`handler/order.go:208`，`strings.Contains(err.Error(), "component unavailable")`）——文案一改 501 静默退化成 500。
- `limit` 无上界（`:51`、`:126`），单次请求可打满表；且该接口无鉴权。
- `internal/handlecheck` 是与 `handlesrc` 逐行同构的死包（整文件），无 import 点。
- 五处注释仍写「两段式 `cache.<Op>`」，与实现的三段式矛盾。
- `messaging.Publish` 的文档注释错位到 `begin` 上，且「producer 为空返回 error」是假承诺（实际会 panic）。

### 值得肯定

- 三层错误判据是设计出来的：`repository`/`docs` 用 `quiet` 豁免 NotFound、`service`/`handler` 不豁免，`service.mark` 把降级与请求失败分开（不改 span 状态但打 Warn + event + 属性）。
- `handlesrc` 在装配期逐组件核对句柄来源与入链，并对各 SDK 的判据强度做诚实分级（dbx 最强、kvx/mqx 只是开关链代理）。
- 测试基建不留假绿：降级用例开工前先 `requireDeadPorts` 校验前提；E1 用 `docker exec` 直查 mysql 绕开缓存证明真落库。


## 5. examples/mall

（65 个 Go 文件 / 14.6k 行，24 条问题：5 必须 / 12 建议 / 7 参考）

### [必须修复]（除 §1 的三条同源项外）

- **释放/确认/盘点路径既无锁也无唯一键**（`srv-stock/internal/service/stock.go:225`、`:245`、`:265`）：只有 `ReserveStock` 走了 `withProductLock`。并发副本能读到同一行流水并各扣一次 `reserved`，`stocks.reserved >= ?` 挡到 0 但挡不住「两次各扣一次」。
- **跨域异步链路只有生产端、没有消费端**（`pkg/events/events.go:6-8` vs `docs/verify.md:118-119`）：注释声明「order.completed.v1 → srv-points 消费」，但全仓 grep 不到任何 `Subscribe`；四个 topic 建好后消息无人接收，outbox 持续膨胀。建议补一个最小消费端，或把注释改成「消费方为后续工作」。

### [建议修改]（摘录）

- 库存补偿基于「不确定结果」（`srv-bff/internal/handler/mall.go:215-219`，同 micro）。
- 幂等命中的重复入账被记成失败（`:236-238`）：积分侧返回 5003 语义是「已入账已忽略」，却被打 `biz.points_failed=true`。
- 商品下架状态不是下单前置条件（`:182-190`）：已下架商品仍可下单，`statusOf` 里 `CodeProductOffShelf` 那行是死分支。
- **关键分支零单测**：本次重构新增的 bff/gateway/handlecheck/routing 四块没有任何 `*_test.go`，`statusOf` / `compensate` / `settle` 只靠 e2e 手跑。
- 限流主体退化为 0 号共享桶（`srv-product/internal/rpc/product.go:92`、`:106` 等 `subjectOf(req.GetUserId(), 0)`）：全体调用方共享一份配额。
- `schema.sql` 硬编码应用账号口令（`:40`，`IDENTIFIED BY 'hubx123'`），与四份 config.yaml 的 `${HUBX_DB_PW}` 口径相反。
- kvx 热度/下单计数键无 TTL（`srv-product/.../product.go:326`、`srv-order/.../order.go:329`），永久堆积；同文件 `bumpFail` 却补了 TTL，三处口径不一致。
- 网关出站复用被 `http.DefaultTransport` 的 `MaxIdleConnsPerHost=2` 卡住（`srv-gateway/internal/proxy/proxy.go:139`）。
- **bff/gateway 的 span 名偏离约定**：bff 是两段式 `handler.GetProduct`，gateway 是 `gateway.proxy.mall`（entity 位放 route name、op 位缺失）。
- **分层重构只覆盖四域 srv**：bff 直接 `engine.GET/POST`、gateway 直接 `engine.Any`，都没接 `pkg/routing` 的路由表与中间件链 ⇒ 最需要自保护的 HTTP 入口层反而是六个进程里唯一没有 audit/rate_limit 的。
- `verify.md:158` 说 producer topic 读 `producer.topic`，config.yaml 说是 `consumer.topic`（后者对）——一份排障提醒自己成了故障源。
- README 声称「多表操作全部挂在可达路径上」，但 `UpdateStatus` 没有 RPC 入口（service 层可达、route 表未登记）。

### 值得肯定

- 防超卖双闸 + `classifyMiss` 双码 + `auditDrift` 对账出口，细节做对了。
- 降级姿态全部显式且写清后果：`withProductLock` 在锁器为 nil 时打 Warn 并写 "oversell is possible"；outbox 在 producer 为 nil 时拒绝启动。
- JOIN 纪律一致且注释说明了代价：`COUNT(DISTINCT)` 抵消行放大、`COALESCE(SUM(...),0)` 处理 NULL、退款额用标量子查询绕开笛卡尔积。
- 六进程端口与配置逐项自洽（9101-9104 / 9201 / 9200，metrics 9131-9134），拓扑与文档一致。

---

## 6. examples/mesh

本组在**上一轮已做过完整评审并修完**（`examples/mesh/docs/code-review-2026-10-10.md`，判定通过）。本轮做了抽样复核：

- ✅ `auth.require=false` 的缺口已按条件挂载修好（`apps/internal/bff/app/setup.go:162`，`opts` 按需 append `WithRequiredIdentity`），并对齐了 `micro/bff/app/setup.go` 的语义。
- ✅ `pkg/routing` 的四向测试（`routing_test.go:105-118`）覆盖了「开闸门+匿名→401」「豁免→放行」「未开闸门+匿名→放行」。
- ✅ `bring-up.sh cmd_verify` 尾部的 `|| true` 已去除，判据退出码即结论。

**遗留（上一轮已知、本轮不改变结论）**：

- `micro` 对照组已失效（`deploy/micro/10-configmap.yaml` 仍是 3 进程拓扑、bff 块缺 `auth`/`demo`/`upstream`），已加 `guard_micro_variant` 硬拒绝（exit 3）+ 文档收窄口径，但**未就地改建** —— 建议单列一个变更处理，不要在本轮混着改。
- 入口闸门的**装配级**单测仍缺（`internal/bff/app` 无 `_test.go`），目前只在 handler 级 + 集群 verify 覆盖。
- 网格只接住 `gateway→bff` 这一跳，`bff→srv` 走 Kitex TTHeader/TCP 永无 sidecar span —— 这是协议事实，不是缺陷，但建议在各案例 README 的统一章节写明，免得被当成漏埋。

---

## 7. 非 Go 面（脚本 / 部署 / CI / 文档口径）

（27 条：10 必须 / 11 建议 / 6 参考）

### [必须修复] 判据失效类（这一类最危险：闸门绿得没有依据）

1. **`ci.sh:83` 的 bash 3.2 中文变量门禁正则恒不命中**。正则 `'\$[A-Za-z_][A-Za-z0-9_]*[^[:space:][:print:]]'` 依赖「非 ASCII 不属于 `[[:print:]]`」，实测 `printf '（x\n' | grep -cE '[^[:print:]]'` = **0**（`[[:print:]]` 把汉字判为可打印），任何 UTF-8 locale 下永远 0 命中。改用 `[^ -~]` 后实测命中 4 处，其中 1 处是真实违规（见第 2 条）。本仓自己登记的头号静默错误失去了唯一一道自动防线。
2. **`examples/mesh/scripts/bring-up.sh:382` 违反该约定**：`echo "  ✗ 无凭据返回 $noauth，期望 401…"`，`$noauth` 后紧跟中文全角逗号。bash 3.2 下静默展开成空串；若该脚本哪天加 `set -u` 就直接 unbound 退出——而它恰好发生在「鉴权没生效」这个最需要可读结论的时刻。
3. **`examples/ACCEPTANCE.md:1056` 与 `examples/HANDLE-AUDIT.md:20` 的红线自检用了 BRE `\|`**：实测 `grep -rn "127\.0\.0\.1\|localhost" --include='*.go' examples/micro/` 命中 **0**，而 `-E` 命中 **2**。这两条是「地址不得硬编码」的红线，在当前工具链下永远输出空 ⇒ 「必须为空」永远满足 ⇒ 把违规判成合规。建议统一改 `-E`，并把这两条固化进 `ci.sh`，不再依赖人手执行文档里的命令。
4. **`examples/ACCEPTANCE.md:1055` 的判据命令指向三个不存在的路径**：`/app/config.yaml`（实际 `/app/conf/<svc>.yaml`）、`examples/mesh/configs/`（实际 `apps/conf/`）、`host.minikube.internal`（实际 `host-middleware.launchx-mesh.svc.cluster.local`）。按文档执行只会得到「文件不存在」，判据既不通过也不失败。
5. **`examples/micro/srv-order/scripts/run-e2e.sh:211` 用 `-lt` 比较且吞掉 stderr**：`if [ "$row" -lt 1 ] 2>/dev/null`，`row` 为空时 `[ "" -lt 1 ]` 返回 2 被 `2>/dev/null` 吃掉 ⇒ 不报错、不断言、继续执行。E3「订单明细落库」这条断言在数据库查询失败时表现为通过。

### [必须修复] CI / 清单不可用类

1. **`examples/scripts/fetch-sdks.sh:60` 给嵌套子模块拼出的 clone URL 含斜杠**：`$r` 形如 `cachex/memcached`，拼成 `https://github.com/gospacex/cachex/memcached.git` 必然 404；而脚本 :31-32 自己写着「仓库名是最后一段」。`.github/workflows/ci.yml:43-45` 第一个 step 就调它 ⇒ 门禁在编译前就挂。
2. **`.github/workflows/ci.yml:74-86` 的 `no-mode-branch` job 不拉兄弟 SDK**：根包 import 的十个 `*x` 全被 `replace` 到 `../<sdk>`，工作区没有这些目录时 `go test .` 在模块解析阶段就失败。这个 job 的定位是「任何环境都必须出结论」，结果任何环境都出不了结论。
3. **`examples/micro/deploy/micro.yaml` 六个 Deployment 完全没有 volumes/volumeMounts**，ConfigMap 里只有一份配置（其余五份被注释成「用 kubectl create configmap 生成」），镜像内也无 config.yaml ⇒ 六个 Pod 全部起不来，与文件头注「二进制与 config.yaml 缺一不可」直接矛盾。
4. **`examples/mesh/deploy/shared/20-host-middleware.yaml:69` 的 Endpoints IP 硬编码**（`192.168.65.254`），注释声称「由 bring-up.sh 替换」但脚本里**没有任何替换或校验逻辑**。换机器后 mysql/redis/kafka/OTLP 全连不上，而失败现象分散在四个中间件上。
5. **`examples/mesh/deploy/apps/40-gateway.yaml:44-47` 在非幂等写路径上开了 retries**：`retryOn: 5xx` 作用在 `prefix: /api` 整条 match 上，而 `POST /api/order` 正是下单写路径；同一套配置自己的注释（10-configmap.yaml:129-133）写着「链路上有非幂等写，传输层超时重试会产生重复落库」。建议按方法拆分 match（GET 保留、POST 不带）。

### [建议修改]（摘录）

- `ci.sh:106` 的 gofmt 步骤不检查 gofmt 自己的退出码，命令替换失败时 `unformatted` 为空 ⇒ 打印 "OK"。唯一失败姿态是假绿。
- `order_db.sh:135` 用 `|| true` 吞掉 ALTER 失败，而 `cmd_check` 只核对列是否存在 ⇒ 补丁没打上但 check 全绿。
- `bring-up.sh:426-428` 显式传 `--service bff`，绕开了 `trace-audit.py` 自己警告过的默认值（只查 bff 会被健康探针的 trace 挤掉）。apps 变体应不传或显式传五服务。
- `auth-probe.sh:134-137` 断言「正确令牌返回 200」却不灌种子数据，单独跑会因 404 判 FAIL。
- `bring-up.sh:261` 把 consul 镜像的 pull 失败吞掉（`|| true`），而它是清单硬依赖。
- `micro.yaml` 的 readinessProbe 打的是 liveness 端点（`/healthz/live`），与同文件头注的论证相反；mesh 组 `30-bff.yaml:92` 用的是 `/healthz/ready`。
- `micro.yaml` 六个容器全无 `securityContext`，与 mesh apps 组的加固水位差距过大；`Dockerfile` 也没有 `USER`。
- `micro.yaml` 一个 Service 都没有，采集口与排障都没有稳定的集群内名字。
- `Dockerfile:39-41` 的二进制清单已过时（仍写 `demo-srv` / 三个 Deployment，实际六进程）。
- `mesh/README.md:106-109` 的地址换算表写 `host.minikube.internal`，与实际清单不符；`:181` 的 T13 描述（6+1 条）与实际 8 行矩阵矛盾。
- `order_db.sh:3-8` 自称「唯一 DDL 真源」，但 `monolith/scripts/bring-up.sh:44-76` 仍是一份完整 DDL，且表结构更旧 ⇒ 谁先跑谁赢，正是该文件声称已消除的故障模式。

### 值得肯定

- 门禁逻辑单一来源：`ci.yml` 只管环境、逻辑全在 `ci.sh`，且显式列出 `examples/mesh/apps` 这类独立 module（并写明「不列出来就等于门禁绿了而那个模块根本没被编译过」）。
- 判据的抗自欺设计：`bring-up.sh:371-389` 把反向断言放在矩阵之前、失败即退，并明确写「不得吞掉 trace-audit 的非零退出码」。
- K8s 加固有理由不照抄：`60-pdb.yaml` 论证了「单副本配 PDB 会让 drain 永远卡住」，并列出多副本前的三个数据正确性前提。
- 诚实标注未交付项：mesh README 把 micro 对照组的失效写成硬拒绝而非静默坏掉；`05-secret.yaml` 坦白明文口令只是教学占位并给出生产形态。
- 两处最容易撒谎的声明实测都成立：`apps/10-configmap.yaml` 自称「逐字镜像」，五份与 conf 逐行相等；micro 对照组失效的自述与清单现状一致。

---

## 8. 值得肯定的地方（先扬后抑）

1. **「为什么」的密度罕见地高**。退出链为什么每一环都要限时、为什么不能用内嵌 `coordx.Register`、为什么 typed nil 必须走反射判、为什么 presence 绝不能搬进 wire 图——这些不是风格注释，是踩坑记录。它们是这个仓库最值钱的资产。
2. **把「无声失效」变成可执行断言**：`handlesrc` / `handlecheck` 在装配期核对句柄来源与入链；`auth-probe.sh` 的反向断言；`degrade_test.go` 开工前先 `requireDeadPorts` 校验前提；`bring-up.sh` 的 401 反向断言。这类设计防的是「看起来绿其实没验到」。
3. **降级一律带后果文案**：`ConsequenceDB` / `ConsequenceLock` / `ConsequenceKV`… 写的是「接下来会发生什么」而不是「出错了」，注册与发现的方向还分开写。运维能从日志直接判断该查什么。
4. **诚实**：micro 对照组失效写成硬拒绝、Secret 明文标注为教学占位、verify.md 自认「现在只有 outbox 事件，没有消费方」。没有粉饰。

---

## 9. 建议的修改路线（按批次）

**第一批（阻断合入，约 10 项）**

1. 库存预占幂等：唯一键 + 事务内查重（micro / mall 两处同改）
2. 下单幂等键：`CreateOrderReq` 加 `order_no` 并透传，订单域事务内查重
3. 归属校验：monolith `List` 加 user 过滤、micro `GetAccount` 改用 `userIDOf`、mall bff/gateway 挂鉴权
4. monolith 删除路径加事务 + `RowsAffected` 判定
5. monolith 错误响应脱敏
6. monolith 挂载 route 层并补断言
7. 四条判据失效：ci.sh 正则、ACCEPTANCE/HANDLE-AUDIT 的 BRE、run-e2e.sh 的 `-lt`、ACCEPTANCE 判据路径
8. CI 不可用：fetch-sdks URL、no-mode-branch 缺 SDK
9. 40-gateway 的 retries 从 POST 路径摘掉
10. outbox 发送 ctx 加超时（退出链无界的根因）

**第二批（本次迭代，约 15 项）**：计数竞态补 `Expire`、缓存穿透回填、释放/确认加互斥、库存熔断只计业务失败、四域错误码统一、`statusOf` 补映射、mall 异步消费端、网关发现缓存与连接复用、micro.yaml 挂载与探针、micro Secret、文档口径（README 地址表 / order_db 真源 / T13 行数）。

**第三批（技术债）**：四域公共骨架上提 `examples/pkg`（`settle` / `rejected` / `RateLimiter` / outbox 装配）、死包清理（`handlecheck` / `pipeline.Shutdown` / `events.Topic*`）、框架层采样率可配、`selfAddr` 空 host 判空、`startMetricsListener` 注释与顺序对齐。

**需要作者确认的 [问题]**：

- `examples/micro/srv/` 冻结目录与 `monolith/scripts/bring-up.sh` 的旧 DDL：是继续冻结（文档标注「历史实现，不随演进」），还是排期统一？
- `examples/monolith` 里无鉴权的 `POST/PUT/DELETE /orders`：是有意保留验收脚本入口，还是应当纳入 `/secure`？若保留，建议把「demo-only」落成可执行约束（`route.Entry` 加 `Insecure` 字段 + 非本地地址拒绝注册），而不是口头约定。

---

## 10. 修复状态回写（2026-10-11）

本章记录「按第 9 章修复」之后的实测状态。**写回时以代码现状为准，不以记忆为准** —— 复核过程中发现两条「以为修完了」的条目其实没有落地（见 10.1 第 2 项与 10.3）。

### 10.0 门禁现状（实测）

| 检查 | 结果 |
|---|---|
| `gofmt -l .` | 空 |
| `go build ./...` | OK |
| `go vet ./...` | OK |
| `go test . ./internal/... ./examples/pkg/...` | 全 ok（含本轮新增 `examples/pkg/cacheutil`） |
| `go test ./examples/mall/srv-points/...` | 全 ok（消费端 11 组） |
| `examples/mesh/apps`（独立 module）build+vet+test | 全 ok |
| `bash examples/scripts/ci.sh` | **门禁 PASS** |

### 10.1 第一批（阻断合入，10 项）

| # | 条目 | 状态 | 落点与判据 |
|---|---|---|---|
| 1 | 库存预占幂等 | ✅ | micro `srv-stock/.../stock.go:221`、mall `:220` 均在事务内先查 `(order_no, product_id)` 流水，命中即 `Idempotent=true` 回既有结果；唯一键：mall `scripts/schema.sql:242`、micro `scripts/schema-micro.sql:34-53`（幂等 ALTER 补丁） |
| 2 | 下单幂等键 | ✅ | 见 10.6（本轮补全：pb 字段 + bff 透传 + 订单域裁决 + 仓储查重 + 四组判据） |
| 3 | 鉴权只认证不授权 | ✅ | micro `bff/.../mall.go:159` `GetAccount` 改 `userIDOf`；mall `srv-bff/app/setup.go:246` 在路由注册**之前** `engine.Engine.Use(auth.Middleware(...))`，凭据缺失即报错不起；monolith `handler.List` 走 `userIDFrom(c)` → `service.List(userID)` 按归属收敛（`service/order.go:157-173`） |
| 4 | monolith 删除事务化 | ✅ | `DeleteWithItems`（`repository/order.go:159`）事务内删主子，`RowsAffected==0` → `ErrNotFound` → 404（原先任何单号都返回 200） |
| 5 | monolith 错误脱敏 | ✅ | `failureOf` 单一出口（`handler/order.go:263`），unknown → 500 + `"internal error"`；新增 `TestFailureOfLeaksNoInternalDetail` 钉住「不得回吐驱动原文」 |
| 6 | monolith 挂载 route 层 | ✅ | `setup.go` 调 `mountRouteLayer(engine.Engine, ...)`；`app/route_layer_test.go` 三条（静态判据 + span 断言 + 未登记回退） |
| 7 | 四条判据失效 | ✅ | `ci.sh` 正则改 `[^ -~]`；`ACCEPTANCE.md` / `HANDLE-AUDIT.md` 的 BRE `\|` 改 `-E`；`run-e2e.sh` 的 `-lt` 改显式判空；判据路径改实际清单并**收窄作用域**（逐个服务目录列举 + 排除 scripts/e2e/kitex_gen） |
| 8 | CI 两项不可用 | ✅ | `fetch-sdks.sh` 嵌套子模块按路径末段取仓库名；`ci.yml` 的 `no-mode-branch` job 补拉兄弟 SDK |
| 9 | 40-gateway 的 retries | ✅ | `40-gateway.yaml:44-47` 按方法拆 match：`POST /api` 单独一条且**不带 retries**，GET 保留 |
| 10 | outbox 退出链无界 | ✅ | `Config.SendTimeout`(5s) / `StopGrace`；`sendOne` 用 `WithTimeout` **派生**传入 ctx（保 traceparent）；`Stop()` 等 `done` 或 grace 超时 + Warn；新增 2 条用例 |

### 10.2 本轮追加（第 9 章「第二批」里的低风险项）

- **monolith `limit` 上界**：新增 `limitOf`（缺省 20、上界 200，非法值回落默认），`List` / `Search` 两处接入；README 接口表补口径。原实现 `strconv.Atoi` 后原样下传，`?limit=10000000` 一次拉满表，而这两个接口都不在鉴权分组下。
- **monolith `messaging.Publish` 的假承诺**：包注释曾写「producer 为空时返回 error」，实现是 `o.producer.Send(...)`（nil 接口 → panic），且注释错位挂在 `begin` 上。改为返回 `ErrProducerUnavailable`（投递器为 nil 时也返回），注释归位到 `Publish`。
- **micro `statusOf` 补映射**：幂等命中 5003/2004 → **200**（渲 5xx 会诱导调用方重试一个必然撞幂等键的请求）；4005/3005 → 409；熔断 4007 → **503**（500/502 不带退避语义）。
- **mall bff handler 单测**：`srv-bff/internal/handler/mall_test.go` 7 组表驱动（`statusOf` 全码、`compensate` 视 4005 为成功、`settle` 分流等）—— 此前这块零单测。
- **mall 异步链路注释如实**：`pkg/events/events.go` 从「→ srv-points 消费」改成设计意图 + 明确「当前只有生产端、没有消费端」。
- **限流 TTL 竞态（部分修）**：抽 `ensureTTL`，先 `TTL` 判一次再写，把覆写窗口从「每次窗口滚动」压到「仅首次」，并给失败留 Warn + span 属性；注释明写**这不是彻底修**（kvx 无 `Expire`，判空与写入之间仍有窗口），降级方向是「限流偏松」。
- **框架层**：`Bootstrap` 不再吞 `Close()` 错误；metrics 监听的关闭钩子移到 `server.stop` 之后（注释同步改如实）；`selfAddr` 拒空 host；采样率可配（`trace.sampler*`）；退出链步骤名常量化（`pipeline.Step*`）。
- **部署面**：`micro/deploy/micro.yaml` 补 6 个 Service、12 处 `securityContext`、readiness 改 `/healthz/ready`、补 volumes/volumeMounts。

### 10.3 未修（含理由与裁定请求）

| 条目 | 状态 | 理由 / 需要作者决定的点 |
|---|---|---|
| **mesh apps 的同源下单幂等** | ✅ 已补齐（见 10.8） | 原判断「没有幂等键生成点、补它等于新增能力」**不成立** —— 复核发现更糟的事实：订单号是 service 层现编的 `M%s%06d`（时间戳 + `userID%1000000`），**同一用户同一秒下两单必然撞号**。现已按 micro/mall 同形补齐：入口生成键 → pb 透传 → 订单域事务内查重（含 outbox 事件不重插） |
| mall 异步消费端 | ✅ 已补齐（见 10.9） | 原判断「需 kafka 真机才能验收，先只改注释」只对了一半：注释如实化是对的，但消费端本身可以落成**传输无关**的逻辑 + 注入替身的判据，真机验收只是最后一步。现已补 `order.completed.v1 → 送积分`，其余三个事件仍只有生产端（并在注释里逐个标明） |
| `pipeline.Shutdown` 零调用 | ✅ 已收敛（见 10.10） | 采纳「让 `Close` 把七步包成 `[]Step` 交给它」：骨架（事件记录 / 幂等 / 错误聚合）只剩一份，各环差异（有界等待 / 15s 冲刷 / logger 吞错）留在 `Step.Fn` 里 |
| 缓存穿透回填空值哨兵 | ✅ 已统一（见 10.11 / 10.12） | 先抽共享封装 `examples/pkg/cacheutil`（含空值哨兵），**全仓 11 处**读缓存全部改走它 —— 上一轮停在 micro/mall 八处的理由（两侧缓存抽象不齐）不成立：收口的是读策略不是连接，把参数换成窄接口 `Store` 即可；顺带补上「Create 后失效」（仅单号可由调用方带来的那两域） |
| monolith 写接口鉴权 | ✅ 已落成（方式与第 9 章建议不同，见 10.7） | 复核时先发现一件更糟的事：`repository/order.go` 与 `service/order.go` 的注释都写着「该路径已被 RegisterRoutes 限制为只能绑回环地址（见 app/routes.go 的 loopbackOnly）」，而全仓 grep `loopbackOnly` **零命中** —— 注释虚构了一条安全约束。现已补成真判据 |
| `micro/srv/` 冻结目录 | ✅ 裁定：维持冻结 + 上判据 | 删了就失去「拆分前 vs 拆分后」的对照证据；统一到 `srv-*` 也没有净收益。冻结语义落成 `frozen_test.go` 两条判据（声明被删会红 / 进部署清单会红），不再是口头约定 |
| monolith 旧 DDL 与真源漂移 | ✅ 裁定：撤下（误列） | 复核后不成立：`bring-up.sh` 只建 3 表是因为 monolith 的业务面就只有 orders / order_items / users（无 outbox、无 stock/points/product），而 `order_db.sh` 的 10 表是给 micro/mall 四域用的 —— 两者不是同一张库的两个版本。且 monolith 的 `orders` 已带 `uk_orders_order_no`，与幂等口径一致 |
| **mall gateway 不重复挂鉴权** | ✅ 裁定：不改 | 报告 §1 第 3 项提到「mall bff/gateway 挂鉴权」，复核后**只在 bff 挂**：mall 的 `pkg/auth` 包注释已论证「转发时逐跳头只剔 Connection/Proxy-*，`Authorization` 会原样透到 bff，故网关侧不需要再验一次」。网关只转 `/api → srv-bff`（`config.yaml:32-37`），判决点唯一且在 bff；网关再挂一份等于两张凭据表、两处轮换 |

### 10.4 反向探针（判据不是空壳的证据）

「判据写完必须至少反向验证一次：故意破坏修复 → 测试必须红 → 还原」。本轮实测：

| 判据 | 破坏方式 | 实测结果 |
|---|---|---|
| outbox `SendTimeout` | `WithTimeout` → `WithCancel` | FAIL（3s 未返回） |
| outbox `StopGrace` | grace 改 30s | FAIL（`Stop` 实测 30.0007s） |
| monolith route 层挂载 | 注释掉 `setup.go` 里的调用 | FAIL（静态判据立刻红） |
| monolith `limit` 上界 | 去掉截断分支 | FAIL（`limitOf("10000000") = 10000000`） |
| monolith producer 缺席 | 去掉 `o.producer == nil` 守卫 | **panic**（比断言失败更严重 —— 正是旧实现的失败姿态） |
| 订单域幂等查重（micro） | 幂等分支改 `false &&` | FAIL（重放返回 error，orders 变 2 行） |
| pb 幂等键的序列化 | `OrderNo` 的 tag 改 `protobuf:"-"` | FAIL（`unknown tag type`，往返丢字段） |
| bff 是否真传了幂等键 | 注释掉 `OrderNo: orderNo` 一行 | 首版判据**没红**（命中注释里的同名串）→ 收紧为逐行匹配后 FAIL |
| 订单域用谁的单号 | `resolveOrderNo` 改无条件生成 | FAIL（调用方给的号被丢弃） |
| monolith 回环判定 | `loopbackOnly` 改恒 `true` | FAIL（对外监听不再拦，两条用例一起红） |
| monolith 监听地址来源 | `cfg.GetString("webx.addr")` 换成字面量 | FAIL（静态判据红 —— 写死回环字面量会让裁决形同虚设） |
| monolith 鉴权路径前缀 | `TrimPrefix(..., "/secure")` 换成不摘前缀 | FAIL（注册出 `/secure/secure/orders`） |
| `micro/srv` 冻结声明 | README 里「冻结」全替换成「停用」 | FAIL（声明被删/改写即红） |
| `micro/srv` 不上线 | 往 `deploy/micro.yaml` 加一行 `image: ...micro/srv:v1` | FAIL（冻结样例进部署清单即红） |
| mesh 订单域查重 | 幂等分支改 `false &&` | FAIL（重放返回新 ID，orders 变 2 行） |
| mesh 用谁的单号 | `resolveOrderNo` 改无条件生成 | FAIL（调用方给的号被丢弃） |
| mesh pb 幂等键序列化 | `OrderNo` 的 tag 改 `protobuf:"-"` | FAIL（往返丢字段） |
| mesh bff 是否真传了键 | 整行注释掉 `OrderNo: orderNo,` | FAIL（逐行判据红 —— 用 `strings.Contains` 会命中注释、假绿） |
| mesh 单号是否带随机段 | 去掉随机段（只留时间戳） | FAIL（2000 次生成出现重复） |
| 退出链走 `pipeline.Shutdown` | 换成等价但不加 `pipeline step` 前缀的内联实现 | FAIL（判据依赖的前缀消失） |
| micro 不引用 mesh | 往 `micro/srv-order/.../repository` 加一个真 import mesh 的文件 | FAIL（真红后还原） |
| 八处读缓存都走 `cacheutil` | 把 points 域改回 `cacheutil.FetchProbe(` | FAIL（静态判据点名该文件） |
| 空值哨兵真的回填 | 删掉 `Fetch` 里的空值回填那一行 | FAIL（`TestFetchFillsNegativeOnNotFound` + `TestFetchNegativeHitKeepsNoCause`） |
| 坏消息是丢弃而非重投 | 丢弃分支 `return nil` 改 `return errors.New(why)` | FAIL（7 个子用例全红） |
| 消费循环停止有界 | `Stop` 超时分支后加 `select {}` | FAIL（用例挂住 2s 后报「又变回无界了」） |
| 订阅侧消费组显式命名 | `group_id: srv-points` 改成 `mqx-group` | FAIL（判据认得缺省值） |
| 残缺缓存值不得当命中 | `Validatable` 分支前加 `false &&` | FAIL（半截对象被当成命中返回） |
| mesh 两域声明 `Valid()` | 删掉 `Product.Valid()` | FAIL（静态判据点名该文件）—— **注意 mesh apps 照样编译通过**，删了不报错，只有判据能抓 |
| monolith typed nil 判空 | `store()` 里去掉 `if s.cache == nil` | FAIL（运行时判据 + 静态判据同时红） |
| 判据能分辨调用形态 | mesh order 的 `FetchResult` 改成 `Fetch` | FAIL（静态判据点名该形态） |

**判据自己也会假红**：第一版按 `:= cacheutil.Fetch(` 全文匹配，把 monolith（`out, err = ...`，具名返回值用 `=`）判成"没接线"。已改成逐行扫 + 跳过注释行。判据太窄会逼人为了过判据改代码 —— 这是判据的错，不是代码的错。

### 10.5 判据连锁反应（修判据不是终点）

把 `HANDLE-AUDIT.md` / `ACCEPTANCE.md` 的地址判据从 BRE `\|` 改成 `-E` 后，**命中从 0 变 8** —— 这 8 处一直存在，只是被失效判据伪装成合规：

- `mall/e2e/main.go` 6 处硬编码 const → 改 `envOr("MALL_*_ADDR", ...)`（缺省值不变）；
- `monolith/internal/handler/order.go` 注释里的驱动报错串 → 改 `<db-host>:<port>` 占位符；
- 同时暴露判据**作用域过宽**：红线本意是「服务进程不得写死监听/注册地址」，原判据却扫 `examples/` 整树，把排障/验收工具的本地默认值也扫进去 ⇒ 恒红 ⇒ 会被当噪声忽略（比漏扫更危险）。已收窄为逐个服务目录列举 + 排除 `*/scripts/**`、`*/e2e/**`、`kitex_gen`、`_test`；收窄后 0 命中。

⇒ **修判据第一次真正生效时，往往会把长期被掩盖的违规一次性吐出来。** 收窄与放宽都要写清理由，否则下一次「顺手放宽」又会把它变回空壳。

### 10.6 下单幂等键：本轮补完的端到端链路

复核发现「bff 生成的号根本没进订单域」（三份 `kitex_gen` 的 `CreateOrderReq` 都没有 `order_no`，订单域自己另起一个号）后，按下面四段补齐（micro / mall 各一套）：

1. **协议面**：`CreateOrderReq` 增加 `OrderNo`（字段 3）+ `GetOrderNo()`。
   - ⚠️ 这是**手工追加**到 `DO NOT EDIT` 生成码的：仓库内没有 `.proto/.thrift` 源（IDL 在仓外维护），重新生成必须同步加回。已在 struct 注释里写明这件事与后果。
   - **为什么不用 metainfo**（另一条候选）：metainfo 依赖 TTHeader 传输，客户端一旦换传输协议就**无声失效** —— 注入方看不出来、提取方只当没传，而幂等键失效的表现是重复扣款。显式进 IDL 的字段是契约，不是运行时约定。
2. **bff**：下单请求带上与库存预占、积分入账**同一个** `orderNo`（此前三个域各用各的关联号，重放 = 新单 + 新预占 + 被幂等掉的积分）。
3. **订单域**：`model.CreateOrderCmd` 加 `OrderNo` → handler 原样透传 → `service.resolveOrderNo`（给了就用，没给才回落生成并打 `order.idempotent_key_missing=true` + Warn）。
4. **仓储**：`CreateOrderTx` 事务内按 `order_no` 查重，命中即回既有订单（`Idempotent=true`），不插订单、不插明细、**也不插 outbox 事件**。

判据共四组（两侧各一套）：pb 序列化往返（字段真的过线）、`resolveOrderNo` 的取舍与留痕、bff 静态判据（真的传了）、仓储 sqlite 重放用例（真的幂等）。全部经过反向探针。

⚠️ **静态判据首版是空壳**：用 `strings.Contains(片段, "OrderNo:")` 会命中**注释里**的同名串 —— 实测把字段整行注释掉后判据照样绿。已收紧为「逐行 + 去缩进后以 `OrderNo:` 开头」。与 `route_layer_test.go` 那条（函数名会命中自己的定义）是同一类陷阱：**源码级判据必须匹配"只有真接线才有的那一段"**。

### 10.7 monolith 写接口鉴权：落成方式（与第 9 章建议不同）

第 9 章建议「`route.Entry` 加 `Insecure` 字段 + 非本地地址拒绝注册」。复核后**没有照做**，改用「按监听地址裁决」，理由如下。

**为什么不加 `Insecure` 字段**：`Secure` 已经表达了"是否走鉴权"，`Insecure` 是它的逻辑非 —— 两个反向字段必然能构造出矛盾态（`Secure:true` + `Insecure:true` 到底算哪个？）。真正的约束是"对外监听时不得存在免鉴权路径"，用 `!Secure` 判定即可；少一个字段少一类矛盾态。

**实际落点**（`app/routes.go`）：

1. `loopbackOnly(addr)` —— 判定监听地址是否只对本机可见。`127.0.0.0/8` 全段、`::1`、`localhost` 为真；`0.0.0.0`、网卡地址、**空 host / 缺端口 / 非法串**一律按非回环处理（收紧，与 `selfAddr` 拒空 host 同口径）。
2. `RegisterRoutes` 增参 `listenAddr`，非回环时两条硬裁决：路由表里还有免鉴权路径 → 报错中止装配；`auth == nil`（缓存降级 / 令牌为空，连 `/secure` 都注册不上）→ 报错中止装配。
3. 回环下**维持现状**（E1 / T13 的验收入口不变），只在真正对外监听时收紧 —— 否则"旁路边车挂了把主流程带走"，那是注释里明写要避免的姿态。

**顺带修掉的既有 bug**：`e.Group("/secure", auth)` 自带前缀，而表里那条 Entry 的 Pattern 也写了 `/secure/orders`，注册出来的实际路径是 **`/secure/secure/orders`**（hertz 启动日志可见）—— 鉴权路径挂在了谁都访问不到的地址上，`/secure/orders` 一直是 404，而 T13 的判据只验「分组存在 / 无凭据 401」，从不真的请求它，所以一直绿。修法是注册时摘前缀（**不能改表里的 Pattern**：route 层按 `c.FullPath()` 查表，改了会让每笔鉴权请求都记成 `route.order.Unregistered`）。

⇒ 若不修这个 bug，新加的回环约束就是在保护一条**不存在的路径**。

### 10.8 mesh apps 下单幂等：补完 + 顺带修掉必撞的单号

原判断是「bff 侧没有幂等键生成点，补它等于新增能力而不是补接线，建议单列变更」。复核后**推翻**：真正的问题不在"缺生成点"，而在订单号本身。

**必撞的单号**：`service.CreateOrder` 里是

```go
OrderNo: fmt.Sprintf("M%s%06d", time.Now().Format("20060102150405"), in.UserID%1000000)
```

时间精度只到秒、用户段只取后 6 位 —— **同一用户同一秒下两单，号完全相同**。撞号在有唯一键时表现为第二单 duplicate error（用户看到下单失败），没唯一键时表现为两单同号（后续按 `order_no` 查询 / 对账串单）。这比"幂等没打通"更具体，也更难自查：它只在并发下偶发。

**补齐的四段**（与 micro / mall 同形）：

1. `CreateOrderReq` 加 `OrderNo`（字段 3）+ `GetOrderNo()`；同样手工追加到生成码，注释写明"重新生成必须同步加回"。
2. bff 在**入口**生成 `orderNo` 并传入 —— 入口才知道"这是一次重试还是一笔新单"。
3. 订单域 `CreateOrderInput.OrderNo` → handler 透传 → `resolveOrderNo`（给了就用；没给才生成并打 `order.idempotent_key_missing=true` + Warn）。
4. 仓储 `Create` **事务内**按 `order_no` 查重，命中即 `Idempotent=true`，**一行都不插（含 outbox 事件）** —— 只查订单表而漏掉 outbox，会出现"订单没重复、但积分域又收到一次『订单已完成』"，那才是最难查的重复。

**出参类型放 `model` 而不是 `repository`**：service 用窄接口面向 repository 编程（不 import 它），而接口方法签名要求返回类型完全相同，故 `model.CreateOutput` 必须是两边都认的共享类型。

**判据四组**（pb 往返 / `resolveOrderNo` 取舍 / 仓储 sqlite 重放 + 反向语义"不同号各落一单" / bff 静态判据逐行匹配），全部经反向探针验证真红后还原。

### 10.9 mall 异步消费端：第一个真消费方

`order.completed.v1 → 送积分` 现在有消费端了（`srv-points/internal/consumer`）。落点三层：

1. **Handler（传输无关）**：`Handle(ctx, *mqx.Message) error` —— 解析事件 → 调 `PointsWriter.Earn` → 按结果分类计数（processed / skipped / failed）。注入替身即可单测，不需要 kafka。
2. **Runner（启停）**：把 mqx 传输与 Handler 接起来，`Stop` **有界**（超时放弃 + Warn）—— kafka-go 的 `ConsumerGroup.Close` 会卡在 joinGroup 的网络读上且不设读 deadline，无界等待会让进程拖到优雅期耗尽被 SIGKILL。
3. **配置**：订阅侧单起 mqx 实例 `srv-points-order-in`。

**为什么订阅侧不能用 `a.MQConsumer()`**：框架按 `service.name` 装配的那个实例，producer 与 consumer **共用 `consumer.topic` 一个键**（mqx 现状：producer 的目标 topic 就读它）。该实例的 topic 是发送侧的 `points.changed.v1`，拿它去收 `order.completed.v1` 会两头都不对。故另起实例 + 业务侧自建句柄，代价是拿不到框架统一的降级文案 —— 已在装配处补了带后果的 Warn。

**坏消息一律丢弃，不返回 error**：解析不出来的消息每次重投都失败，会让整个分区堵在一条坏消息上（head-of-line blocking），后面成千上万条好消息跟着不处理。没有 DLQ 时的取舍是"丢一条可对账补偿的积分，也不让队列停摆"，故丢弃时打 Error 级日志并计数。同理，schema_ver 高于当前支持版本也丢弃（消费者落后于生产者时，返回 error 等于让分区卡在自己读不懂的消息上）。

**幂等交给下游，消费端不自己记第二份**：重投时照旧调 `Earn`，由 `uk_ledger_order_no_type` 唯一键裁决，命中 `CodePointsDuplicated` 视作**成功**（否则"正常重投"会在计数上冒充故障）。自己再存一份"处理过的 OrderNo"等于把同一事实存两处，两处迟早不一致。

判据 11 组（正常入账 / 7 类坏消息丢弃 / 暂时失败可重试 / 业务拒绝丢弃 / 幂等命中算成功 / 重投不重复加钱 / Runner 接线 / 停止有界 / 正常退出 / nil 降级 / 配置里 `group_id` 非缺省值），三组反向探针见 10.4。

**仍未补的**：`product.created.v1` / `stock.reserved.v1` / `points.changed.v1` 三个事件只有生产端 —— 事件包注释已逐个标明"已交付"与"设计意图"，不再一律写成「→ X 消费」。

### 10.10 退出链收敛：骨架只剩一份

`App.Close` 原本自己实现了「反序 + 事件记录 + 幂等 + 错误聚合」，而 `internal/pipeline.Shutdown` 做的是同一件事 —— 后者**零调用**，是死代码。死代码比重复代码更危险：它看起来像被维护着，改它的人会以为退出链跟着改了。

改法是采纳报告的建议二：`Close` 只负责**编排步骤**，执行交给 `pipeline.Shutdown`：

```go
return pipeline.Shutdown(a.env(), []pipeline.Step{
    stopHooks, stopServer, stopMetricsListener, stopComponents,
    deregister, flushTracing, flushMetrics, closeLogger,
})
```

各环**差异**留在 `Step.Fn` 里，它们不是骨架该管的：

- 业务 / 服务端 / 采集口 / 组件四环额外套有界等待（`runStop`），超时只记 `stop.timeout.<name>` 不进聚合；
- tracing 环有独立的 15s 冲刷上限（批量导出器缓冲未冲刷会静默丢掉"停机过程本身"那批 span —— 恰好是最该被看见的）；
- logger 环刻意吞错：它是记录前面所有失败的唯一手段，不能被自己关掉。

新增常量 `pipeline.StepMetricsListener`（采集口那一环此前只执行不记事件，顺序契约因此缺一环）。判据取**运行时**证据而非 grep：`Shutdown` 给每步错误加 `pipeline step %q` 前缀，断言它存在即证明真走了那份代码；同一条用例顺带钉住完整顺序契约（此前只验了 `server.stop` 与 `deregister` 的先后两条，中间漏掉 `metrics.listener` / `component.stop` 都能溜过去）。

### 10.11 缓存穿透：先统一封装，再补空值哨兵

原判断「8 处各写一份，改一处要改八遍，会抄出八份不同的哨兵」—— 所以**先抽共享封装**再做语义修正，顺序反了就是八份复制。

新增 `examples/pkg/cacheutil`（与 outbox 同层，三示例共用），`Fetch[T]` 收口整条链路：

| 情形 | 行为 |
|---|---|
| 命中正缓存 | 返回，记 `cache.hit` |
| 命中空值哨兵 | 返回 `*NotFound`（**不回源**），记 `cache.hit` + `cache.negative` |
| miss 且回源报 not found | 回填**空值哨兵**（MissTTL，默认 30s）后返回 `*NotFound` |
| 回源报其他错 | 原样上抛，**不写任何缓存** |
| 回填失败 / 解码失败 | 只告警（缓存是加速路径，不是数据源） |
| 缓存组件为 nil | 退化为直接回源（合法部署形态） |

`*NotFound` 包住回源原始错误并 `Unwrap`，因此各域的 `mapRepoErr` / `codes.Wrap` 靠 `errors.Is(err, repository.ErrNotFound)` 照样认得出 —— 命中负缓存与回源 miss 走同一条翻译路径，不会给出两种错误码。

**必须配套的第二步：Create 路径失效**。负缓存的固有代价是"记录创建后最长 MissTTL 内仍查不到"。四域里 stock（首次预占）与 points（首次入账）的写路径已经失效，缺的是 **product 与 order 的 Create** —— 已补 `s.invalidate(...)`。不补的话，"查过一次不存在的 ID → 该 ID 随后被创建"会让新记录在窗口内查不到，等于用修穿透换来了另一个更难查的 bug。

**回源故障绝不能写成空值哨兵**：那会让一次抖动变成"最长 MissTTL 内该键查不到"，且没有任何错误日志，数据看起来凭空消失。这条单独有用例钉住。

判据三层：包内单测（含上面每一条语义）+ 静态判据逐处枚举 + `TestCreatePathsInvalidate`。

### 10.12 收口的最后一公里：monolith 与 mesh apps

上一轮把 10.11 的范围停在 micro/mall 八处，理由是 monolith 与 mesh apps 的缓存抽象与 `cachex.Cache` 不齐（monolith 的 `cache.Order.Set` 不带 TTL 参数）。**这个理由不成立**：收口的是**读策略**，不是连接 —— 把参数从 `cachex.Cache` 换成只含 `Get/Set(带 ttl)/Delete` 的窄接口 `Store`，两侧都能进来。

于是上一轮的"未纳入"三条全部收口，全仓读缓存只剩一份实现（11 处）：

| 落点 | 适配方式 |
|---|---|
| micro / mall 各四域（8） | `cachex.Cache` 天然满足 `Store` |
| monolith `service.Get` | `orderCacheStore` 适配 `*cache.Order`，保留它外面那层业务 span（**不能**改成直接交裸 cachex：Jaeger 上就只剩一条 redis 命令，回答不了"谁在访问缓存"） |
| mesh apps order / product（2） | 直接用 `cachex.Cache`；改用 `FetchResult` 取命中状态 |

过程中撞到并解决的三件事：

1. **typed nil 陷阱**：`s.cache` 是 `*cache.Order`，**nil 指针转成接口后 != nil**，直接交给 cacheutil 会让"没配缓存"这条默认路径在 nil 接收者上调方法。判空必须写在适配处（`(*Service).store()`），并有运行时用例 + 静态判据双份钉住。
2. **"解码成功 ≠ 值完整"不能丢**：mesh 两域此前用 `ID == 0` 挡半截对象。收口时把它做成 `cacheutil.Validatable` 接口（值自己声明完不完整），而不是塞进 `Options`——`Options` 不是泛型的，装不下 `func(*T) bool`；再者"什么算完整"本来就只有值自己知道。
3. **TTL 的真源只有一处**：monolith 的正缓存 TTL 从 `cache.Order.TTL()` 取，不在 service 里再写一份（抄一份会让"改了配置只有一半缓存跟着变"）。

**Create 路径失效不是"写路径都该失效"**：monolith 与 mesh order 的新记录主键/单号由服务端生成，不存在"这个键刚被记成不存在"的前提，故未补失效；micro/mall 那两处补了，是因为它们的单号可由调用方带来（幂等键 / bff 生成）。差别写在代码注释里，免得后来人对着两边以为漏了一边。

**判据为什么不写死 `:=`**：monolith 的调用点是 `out, err = cacheutil.Fetch(ctx, ...)`（具名返回值用 `=`）。第一版判据按 `:= cacheutil.Fetch(` 全文匹配，把**已接线的 monolith 判成没接线**（假红）；改成逐行扫 + 跳过注释行 + 用 `cacheutil.Fetch(ctx,` 区分 `FetchResult`。反向探针：把 mesh order 改成 `Fetch` → 红；把 `Valid()` 删掉 → 红（**而 mesh apps 照样编译通过**，这正是"删了也不报错"的那类契约，只能靠静态判据抓）。
