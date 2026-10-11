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
