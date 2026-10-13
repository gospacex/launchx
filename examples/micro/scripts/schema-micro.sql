-- =============================================================================
-- micro 案例**自有**的 DDL 补丁（幂等，可反复执行）
-- -----------------------------------------------------------------------------
-- 为什么单独一个文件，而不是去改共用的 DDL：
--
-- micro 的四库 DDL 目前来自 examples/mall/scripts/schema.sql（见 e2e.sh 的
-- SCHEMA_SQL）—— 那份是 mall 案例的资产，micro 只是借用。而本文件要加的约束
-- 是 micro 侧缺陷修复的一部分（库存预占幂等），改共用文件会波及另一个案例，
-- 故收敛到这里：micro 的 DDL 真源 = 共用 schema.sql + 本补丁。
--
-- 为什么不能直接写 `CREATE UNIQUE INDEX IF NOT EXISTS`：MySQL（含 8.0）没有
-- 这个语法（那是 PostgreSQL / SQLite 的），写了就是语法错误。故沿用本仓库既有
-- 手法（见 examples/scripts/order_db.sh 的 index_exists）：先查
-- information_schema.statistics，再决定是否 ALTER，用 PREPARE 让"跳过"变成
-- 一条合法语句（DO 0），整份脚本才能无条件重跑。
--
-- 用法（与 e2e.sh 的 mysql_exec 一致，经 mysql 客户端或 docker exec 灌入）：
--   mysql -h127.0.0.1 -P3306 -uroot -p"$MYSQL_ROOT_PW" < scripts/schema-micro.sql
-- =============================================================================

USE stock_db;

-- ---- 1) 库存预占流水的幂等唯一键 ------------------------------------------
--
-- 没有它，「重放一次 POST /order」会插出第二条 (order_no, product_id) 流水：
-- 第二条照常扣一次 available，而释放路径（ReleaseTx 的 `Order("id DESC").Take`）
-- 只认最新一条 —— 旧流水永久停在 status=1，冻结的库存再也放不出来。
-- 应用侧的先查后插（见 srv-stock repository.ReserveTx）在并发下挡不住这个窗口，
-- 唯一键是唯一能在数据库层面把它关掉的手段。
SET @uk_exists := (
  SELECT COUNT(1) FROM information_schema.statistics
   WHERE table_schema = 'stock_db'
     AND table_name   = 'stock_reservations'
     AND index_name   = 'uk_sr_order_product'
);
SET @uk_ddl := IF(@uk_exists = 0,
  'ALTER TABLE stock_reservations ADD UNIQUE KEY uk_sr_order_product (order_no, product_id)',
  'DO 0');
PREPARE uk_stmt FROM @uk_ddl;
EXECUTE uk_stmt;
DEALLOCATE PREPARE uk_stmt;

-- ---- 2) 旧的普通索引由新唯一键完全覆盖，撤掉它 ----------------------------
--
-- idx_sr_order_product (order_no, product_id) 与新唯一键同列同序，唯一键建好
-- 之后它就是纯冗余：留着只会让每次 INSERT/UPDATE 多维护一棵 B+ 树。
-- 判据带上「唯一键已存在」：唯一键没建成（例如库里已有重复流水，ADD UNIQUE
-- 会报 1062）时绝不撤旧索引，否则连原来的查询路径一起没了。
SET @uk_now := (
  SELECT COUNT(1) FROM information_schema.statistics
   WHERE table_schema = 'stock_db'
     AND table_name   = 'stock_reservations'
     AND index_name   = 'uk_sr_order_product'
);
SET @old_now := (
  SELECT COUNT(1) FROM information_schema.statistics
   WHERE table_schema = 'stock_db'
     AND table_name   = 'stock_reservations'
     AND index_name   = 'idx_sr_order_product'
);
SET @drop_ddl := IF(@uk_now > 0 AND @old_now > 0,
  'ALTER TABLE stock_reservations DROP KEY idx_sr_order_product',
  'DO 0');
PREPARE drop_stmt FROM @drop_ddl;
EXECUTE drop_stmt;
DEALLOCATE PREPARE drop_stmt;

-- -----------------------------------------------------------------------------
-- 已知的前置条件（不是本脚本能替你决定的事）：
--
-- 若库里已经存在重复的 (order_no, product_id) 流水，第 1 步会以
-- `ERROR 1062 Duplicate entry` 失败并中止整份脚本 —— 这是**刻意的响亮失败**：
-- 去重意味着删除流水行，而"哪一条是真的"只有业务方知道，脚本替你删就是毁数据。
-- 届请先人工核对（例如按 order_no 分组找出重复项、与 orders 对账后再决定去留），
-- 去重后重跑本脚本即可。
-- -----------------------------------------------------------------------------

-- =============================================================================
-- DTM 增量（add-micro-dtm-saga-tcc）：四域各一份 barrier 表 + 冻结/销量/占位单列
-- -----------------------------------------------------------------------------
-- 列定义**逐字取自 dtm 官方 sqls/dtmcli.barrier.mysql.sql**，一处未改：
--
--   create table if not exists dtm_barrier.barrier(
--     id bigint(22) PRIMARY KEY AUTO_INCREMENT,
--     trans_type varchar(45) default '', gid varchar(128) default '',
--     branch_id varchar(128) default '', op varchar(45) default '',
--     barrier_id varchar(45) default '', reason varchar(45) default '',
--     create_time datetime DEFAULT now(), update_time datetime DEFAULT now(),
--     key(create_time), key(update_time),
--     UNIQUE key(gid, branch_id, op, barrier_id)) ...
--
-- 唯一键**必须**是 (gid, branch_id, op, barrier_id)。早先的草案漏了 trans_type /
-- gid 两列、且把唯一键写成 (branch_id, barrier_id) —— 后者会让不同全局事务里
-- 同名分支互相判重，直接造成跨事务丢账。本文件以官方定义为准。
--
-- 与官方的唯一差异：**表落在各域业务库内（表名 barrier），不建独立 dtm_barrier 库**。
-- 原因是 dtm 的 barrier 行由 BranchBarrier.CallWithDB 在**业务连接的事务内**插入
-- （见 client/dtmcli/barrier.go：tx := db.Begin() 后 InsertBarrier(tx, ...)），
-- 落到同一库里才能与业务写同事务原子提交/回滚。四个域各是独立进程，各自在自己的
-- 连接上执行 `insert ignore into barrier`，天然互不干扰。
-- 装配期由 dtmx.BarrierFromCtx 设 dtmcli.SetBarrierTableName("barrier")；
-- 该变量是进程级全局量，跨进程无共享，四域各自设成同名即可。
--
-- dtm 的 InsertBarrier 对 MySQL 走 `insert ignore into <表名>(...)`，不引用
-- 约束名（GetInsertIgnoreTemplate 的 pgConstraint 参数只在 postgres 分支用），
-- 故唯一键叫什么名字都不影响 MySQL 运行；这里仍命名为 uniq_barrier，与官方
-- 意图对齐，便于将来换 postgres 时不必改表。
-- =============================================================================

-- ---- 1) 四域 barrier 表 ------------------------------------------------------
-- 逐份重复而非一条 `CREATE TABLE ... ` 跨库：MySQL 的 CREATE TABLE 不跨库，
-- 每份前置自己的 USE，与本文件既有写法同构。

USE stock_db;
CREATE TABLE IF NOT EXISTS barrier (
  id          BIGINT      NOT NULL AUTO_INCREMENT,
  trans_type  VARCHAR(45)  NOT NULL DEFAULT '',
  gid         VARCHAR(128) NOT NULL DEFAULT '',
  branch_id   VARCHAR(128) NOT NULL DEFAULT '',
  op          VARCHAR(45)  NOT NULL DEFAULT '',
  barrier_id  VARCHAR(45)  NOT NULL DEFAULT '',
  reason      VARCHAR(45)  NOT NULL DEFAULT '' COMMENT 'the branch type who insert this record',
  create_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_barrier_create_time (create_time),
  KEY idx_barrier_update_time (update_time),
  UNIQUE KEY uniq_barrier (gid, branch_id, op, barrier_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='dtm 子事务屏障（官方定义，按域隔离）';

USE points_db;
CREATE TABLE IF NOT EXISTS barrier (
  id          BIGINT      NOT NULL AUTO_INCREMENT,
  trans_type  VARCHAR(45)  NOT NULL DEFAULT '',
  gid         VARCHAR(128) NOT NULL DEFAULT '',
  branch_id   VARCHAR(128) NOT NULL DEFAULT '',
  op          VARCHAR(45)  NOT NULL DEFAULT '',
  barrier_id  VARCHAR(45)  NOT NULL DEFAULT '',
  reason      VARCHAR(45)  NOT NULL DEFAULT '' COMMENT 'the branch type who insert this record',
  create_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_barrier_create_time (create_time),
  KEY idx_barrier_update_time (update_time),
  UNIQUE KEY uniq_barrier (gid, branch_id, op, barrier_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='dtm 子事务屏障（官方定义，按域隔离）';

USE order_db;
CREATE TABLE IF NOT EXISTS barrier (
  id          BIGINT      NOT NULL AUTO_INCREMENT,
  trans_type  VARCHAR(45)  NOT NULL DEFAULT '',
  gid         VARCHAR(128) NOT NULL DEFAULT '',
  branch_id   VARCHAR(128) NOT NULL DEFAULT '',
  op          VARCHAR(45)  NOT NULL DEFAULT '',
  barrier_id  VARCHAR(45)  NOT NULL DEFAULT '',
  reason      VARCHAR(45)  NOT NULL DEFAULT '' COMMENT 'the branch type who insert this record',
  create_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_barrier_create_time (create_time),
  KEY idx_barrier_update_time (update_time),
  UNIQUE KEY uniq_barrier (gid, branch_id, op, barrier_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='dtm 子事务屏障（官方定义，按域隔离）';

USE product_db;
CREATE TABLE IF NOT EXISTS barrier (
  id          BIGINT      NOT NULL AUTO_INCREMENT,
  trans_type  VARCHAR(45)  NOT NULL DEFAULT '',
  gid         VARCHAR(128) NOT NULL DEFAULT '',
  branch_id   VARCHAR(128) NOT NULL DEFAULT '',
  op          VARCHAR(45)  NOT NULL DEFAULT '',
  barrier_id  VARCHAR(45)  NOT NULL DEFAULT '',
  reason      VARCHAR(45)  NOT NULL DEFAULT '' COMMENT 'the branch type who insert this record',
  create_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  update_time DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_barrier_create_time (create_time),
  KEY idx_barrier_update_time (update_time),
  UNIQUE KEY uniq_barrier (gid, branch_id, op, barrier_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='dtm 子事务屏障（官方定义，按域隔离）';

-- ---- 2) product_db：Saga 销量分支的账变落点 ---------------------------------
-- 为什么新起一张表而不给 products 加一列：销量是**高频写**（每单 +1），
-- 与 products 的低频读（详情/列表）挤同一行会把行锁争用拉到整张商品表上；
-- 且销量需要「可独立回退到基线」的语义，单独成表便于 Saga 补偿断言。
USE product_db;
CREATE TABLE IF NOT EXISTS product_sales (
  product_id BIGINT UNSIGNED NOT NULL,
  sold       BIGINT       NOT NULL DEFAULT 0 COMMENT '累计销量；Saga 销量分支的账变落点',
  updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (product_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='商品销量（Saga 分支账变）';

-- ---- 3) 冻结语义列（TCC Try/Confirm/Cancel） --------------------------------
-- 三态不变式：available + frozen + reserved <= total
--   TryFreeze：available↓ frozen↑（尚未实扣，可 Cancel 回退）
--   Confirm  ：frozen↓（真正实扣，可用永久减少）
--   Cancel   ：frozen↓ available↑（完全回原状）
-- 故列必须是「可售之外另立一档 frozen」，不能复用 reserved —— reserved 绑 order_no
-- 走的是预占流水语义（stock_reservations），与 TCC 的冻结/实扣是两套账。

USE stock_db;
SET @c_exists := (
  SELECT COUNT(1) FROM information_schema.COLUMNS
   WHERE table_schema = 'stock_db' AND table_name = 'stocks' AND column_name = 'frozen'
);
SET @c_ddl := IF(@c_exists = 0,
  'ALTER TABLE stocks ADD COLUMN frozen INT NOT NULL DEFAULT 0 COMMENT ''TCC 冻结量：Try 可售→冻结，Confirm 冻结→实扣，Cancel 冻结→可售''',
  'DO 0');
PREPARE frozen_stmt FROM @c_ddl;
EXECUTE frozen_stmt;
DEALLOCATE PREPARE frozen_stmt;

USE points_db;
SET @pc_exists := (
  SELECT COUNT(1) FROM information_schema.COLUMNS
   WHERE table_schema = 'points_db' AND table_name = 'point_accounts' AND column_name = 'frozen'
);
SET @pc_ddl := IF(@pc_exists = 0,
  'ALTER TABLE point_accounts ADD COLUMN frozen BIGINT NOT NULL DEFAULT 0 COMMENT ''TCC 冻结积分：Try 可用→冻结，Confirm 冻结→实扣，Cancel 冻结→可用''',
  'DO 0');
PREPARE pfrozen_stmt FROM @pc_ddl;
EXECUTE pfrozen_stmt;
DEALLOCATE PREPARE pfrozen_stmt;

-- ---- 4) orders：TCC 占位单与 dtm gid 的关联 ---------------------------------
-- status 已是 VARCHAR(32)，`presale_locked` 是取值而非 schema 变更，无需 DDL。
-- 真正缺的是「这张占位单属于哪个全局事务」：Confirm/Cancel 回调只带 gid 与
-- branch_id，不带 order_no，没有这列就只能全表扫 status 猜是哪一张单。
USE order_db;
SET @og_exists := (
  SELECT COUNT(1) FROM information_schema.COLUMNS
   WHERE table_schema = 'order_db' AND table_name = 'orders' AND column_name = 'dtm_gid'
);
SET @og_ddl := IF(@og_exists = 0,
  'ALTER TABLE orders ADD COLUMN dtm_gid VARCHAR(128) NOT NULL DEFAULT '''' COMMENT ''所属 dtm 全局事务号；Saga/TCC 占位与补偿按它定位''',
  'DO 0');
PREPARE og_stmt FROM @og_ddl;
EXECUTE og_stmt;
DEALLOCATE PREPARE og_stmt;

SET @ogi_exists := (
  SELECT COUNT(1) FROM information_schema.statistics
   WHERE table_schema = 'order_db' AND table_name = 'orders' AND index_name = 'idx_orders_dtm_gid'
);
SET @ogi_ddl := IF(@ogi_exists = 0,
  'ALTER TABLE orders ADD KEY idx_orders_dtm_gid (dtm_gid)',
  'DO 0');
PREPARE ogi_stmt FROM @ogi_ddl;
EXECUTE ogi_stmt;
DEALLOCATE PREPARE ogi_stmt;

-- -----------------------------------------------------------------------------
-- dtm server 自己的事务表（trans_global / trans_branch_op / kv）**不在本文件**：
-- 归属 dtm_db，由 dtm server 的官方 DDL sqls/dtmsvr.storage.mysql.sql 建，见
-- Task 2 的执行记录。业务四库里不得出现 trans_* 表。
-- -----------------------------------------------------------------------------
