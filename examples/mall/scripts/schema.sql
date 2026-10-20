-- ============================================================

-- ---------------------------- 版本 ----------------------------
-- SCHEMA_VERSION 每次改表结构都必须 +1，并且**只增不改**：
-- DDL 一旦落地就可能被别的环境先执行过，改历史脚本等于让两个环境的同一个
-- 版本号指向两份不同的结构 —— 那种不一致只在出事那天才被发现。
--
-- 本脚本整体幂等（CREATE TABLE IF NOT EXISTS + INSERT IGNORE），可以重复跑；
-- 每跑一次会在各自库的 schema_migrations 里留一行，用来回答
-- 「这个环境现在到底跑到哪一版了」。
--
-- 查版本：
--   for db in product_db order_db stock_db points_db; do
--     docker exec mysql mysql -uroot -prootpass -N -e \
--       "SELECT '$db', MAX(version) FROM $db.schema_migrations"
--   done
--
-- 一域一库 ⇒ 一库一份版本账：**跨域不共用一张 schema_migrations**。
-- 共用等于把「商品域加了个字段」和「订单域改了个索引」记在同一条时间线上，
-- 回滚时无法只回一个域。
-- mall 示例 · 四域私有库建表脚本
--
-- 纪律：一域一库（product_db / order_db / stock_db / points_db），
-- 跨域只存 ID，不建跨库外键 —— 外键会把"服务边界"变成"数据库耦合"。
--
-- 用法（本机 compose 里的 MySQL，应用账号 mall_rw）：
--
--   ⚠️ 本文件里的口令写成了 `${HUBX_DB_PW:-...}` 占位，**不能直接重定向给
--   mysql**（`docker exec -i mysql mysql -uroot -prootpass < schema.sql`）。
--   重定向不经过 shell 展开，占位会被当成口令本身灌进库里：账号照样建成功、
--   GRANT 照样发、SHOW GRANTS 也正常，唯一症状是应用侧用真口令连得到
--   ERROR 1045 —— 而 1045 不提"灌库姿势"，于是人去怀疑网络、host 通配符、
--   MySQL 8 的认证插件，没人想到库里的口令就是那串字面量。本机 2026-10-14
--   正是这么中招的。
--
--   先渲染再灌（渲染要靠调用方，本文件自己展开不了自己）：
--     · micro 案例：`examples/micro/scripts/e2e.sh` 的 render_schema_sql()
--       已封装（含"渲染后仍有占位就报错"的断言），跑 e2e.sh 自动走上这条路；
--     · 手工场景：照抄上面那个函数里的 sed，或看 examples/mall/README.md。
--
-- ---------------------------- 口令（仅演示）----------------------------
-- 应用账号 mall_rw 的口令**不硬编码在本文件里**：IDENTIFIED BY 处写成
-- `${HUBX_DB_PW:-hubx123}`，由**灌脚本的一方**在灌之前展开。
--
--   变量名为什么是 HUBX_DB_PW：跟四服 config.yaml 里 DSN 的占位同名 ——
--   「建账号」和「连库」是同一件事的两半，用两个不同名字的变量，迟早会喂出
--   两个不同的值（本机旧版用 MALL_RW_PW 建号、HUBX_DB_PW 连库，就是这么炸的）。
--   旧名 MALL_RW_PW 仍作为别名兼容，渲染时优先级 MALL_RW_PW > HUBX_DB_PW。
--
--   ⚠️ 这里的 `hubx123` 是**演示默认值**，不是建议口令。生产口令必须走
--   Secret 注入（K8s Secret / Vault / CI 变量），绝不写进仓库，也别靠缺省值
--   兜底 —— 缺省值存在的唯一理由是"本机一遍跑通"。
--
-- 灌完后这样验证（比 SHOW GRANTS 有用得多，GRANT 是对的也照样能连不上）：
--   docker exec mysql mysql -umall_rw -p"${HUBX_DB_PW}" -h127.0.0.1 -N -B -e "SELECT 1"
--   报 1045 就是上面的展开没做或做错了，不是网络问题。
-- ============================================================

-- ---------------------------- 应用账号 ----------------------------
-- 四服的 DSN 一律用 mall_rw（见各 srv-*/config.yaml），不复用 root：
-- 示例要演示的是"应用只拿到自己库的读写权"，用 root 跑通等于把权限边界
-- 演示没了。幂等：MySQL 8 没有 CREATE USER IF NOT EXISTS，故先 DROP 再建，
-- 重跑本脚本不会报 "user already exists"。
DROP USER IF EXISTS 'mall_rw'@'%';
CREATE USER 'mall_rw'@'%' IDENTIFIED BY '${HUBX_DB_PW:-hubx123}';
GRANT SELECT, INSERT, UPDATE, DELETE ON product_db.* TO 'mall_rw'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON order_db.*  TO 'mall_rw'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON stock_db.*  TO 'mall_rw'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON points_db.* TO 'mall_rw'@'%';
FLUSH PRIVILEGES;

-- ---------------------------- 商品域 ----------------------------
CREATE DATABASE IF NOT EXISTS product_db DEFAULT CHARSET utf8mb4;
USE product_db;

-- 版本账：本库自己的迁移记录（一域一库 ⇒ 一库一份，不跨域共用）
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    VARCHAR(64) NOT NULL PRIMARY KEY,
  applied_at TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='本库已执行的建表脚本版本';
INSERT IGNORE INTO schema_migrations (version) VALUES ('2026.10.07-1');

CREATE TABLE IF NOT EXISTS categories (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  name       VARCHAR(64)  NOT NULL,
  parent_id  BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS products (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  title       VARCHAR(128) NOT NULL,
  price       BIGINT       NOT NULL DEFAULT 0 COMMENT '分',
  status      TINYINT      NOT NULL DEFAULT 1 COMMENT '1 上架 / 2 下架',
  category_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_products_category_status (category_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS product_skus (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  product_id BIGINT UNSIGNED NOT NULL,
  sku_code   VARCHAR(64)  NOT NULL,
  price      BIGINT       NOT NULL DEFAULT 0 COMMENT '分',
  on_sale    TINYINT      NOT NULL DEFAULT 1 COMMENT '1 在售 / 0 停售',
  created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_sku_code (sku_code),
  KEY idx_skus_product (product_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS product_logs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  product_id  BIGINT UNSIGNED NOT NULL,
  action      VARCHAR(32)  NOT NULL COMMENT 'create/update/on_sale/off_shelf/sold',
  detail      VARCHAR(255) NOT NULL DEFAULT '',
  operator_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_plogs_product (product_id),
  KEY idx_plogs_action (action)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- 事务发件箱（四库同名同构：业务行与事件行同事务，补发器 3s 轮询）
CREATE TABLE IF NOT EXISTS outbox (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  topic       VARCHAR(128) NOT NULL,
  event_type  VARCHAR(64)  NOT NULL,
  payload     TEXT,
  traceparent VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'W3C traceparent，随 mqx header 续链',
  -- status 取值与 examples/pkg/outbox 的常量一一对应（改一处须改另一处）：
  -- 0 pending（待发）/ 1 sent（已投）/ 2 abandon（重试上限，转人工对账）
  -- / 3 claimed(在飞)（已被补发器取走、尚未确认结果；进程中途崩溃时靠它
  --   区分"正在发"与"还没发"，避免重启后被重复取走）
  status      TINYINT      NOT NULL DEFAULT 0 COMMENT '0 pending / 1 sent / 2 abandon / 3 claimed(在飞)',
  retry_count INT          NOT NULL DEFAULT 0,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_outbox_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------- 订单域 ----------------------------
CREATE DATABASE IF NOT EXISTS order_db DEFAULT CHARSET utf8mb4;
USE order_db;

-- 版本账：本库自己的迁移记录（一域一库 ⇒ 一库一份，不跨域共用）
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    VARCHAR(64) NOT NULL PRIMARY KEY,
  applied_at TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='本库已执行的建表脚本版本';
INSERT IGNORE INTO schema_migrations (version) VALUES ('2026.10.07-1');

CREATE TABLE IF NOT EXISTS orders (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id      BIGINT UNSIGNED NOT NULL,
  order_no     VARCHAR(40)  NOT NULL,
  status       VARCHAR(32)  NOT NULL DEFAULT 'created',
  total_amount BIGINT       NOT NULL DEFAULT 0 COMMENT '分',
  paid_amount  BIGINT       NOT NULL DEFAULT 0 COMMENT '分',
  created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_order_no (order_no),
  KEY idx_orders_user (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS order_items (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_id     BIGINT UNSIGNED NOT NULL,
  product_id   BIGINT UNSIGNED NOT NULL,
  product_name VARCHAR(128) NOT NULL DEFAULT '',
  quantity     INT          NOT NULL DEFAULT 0,
  unit_price   BIGINT       NOT NULL DEFAULT 0 COMMENT '分',
  PRIMARY KEY (id),
  KEY idx_items_order (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS order_status_log (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_id    BIGINT UNSIGNED NOT NULL,
  from_status VARCHAR(32)  NOT NULL DEFAULT '',
  to_status   VARCHAR(32)  NOT NULL DEFAULT '',
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_osl_order (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS refund_records (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_id   BIGINT UNSIGNED NOT NULL,
  amount     BIGINT       NOT NULL DEFAULT 0 COMMENT '分',
  status     VARCHAR(32)  NOT NULL DEFAULT 'init',
  created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_refund_order (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS outbox (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  topic       VARCHAR(128) NOT NULL,
  event_type  VARCHAR(64)  NOT NULL,
  payload     TEXT,
  traceparent VARCHAR(255) NOT NULL DEFAULT '',
  status      TINYINT      NOT NULL DEFAULT 0,
  retry_count INT          NOT NULL DEFAULT 0,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_outbox_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------- 库存域 ----------------------------
CREATE DATABASE IF NOT EXISTS stock_db DEFAULT CHARSET utf8mb4;
USE stock_db;

-- 版本账：本库自己的迁移记录（一域一库 ⇒ 一库一份，不跨域共用）
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    VARCHAR(64) NOT NULL PRIMARY KEY,
  applied_at TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='本库已执行的建表脚本版本';
INSERT IGNORE INTO schema_migrations (version) VALUES ('2026.10.07-1');

CREATE TABLE IF NOT EXISTS stocks (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  product_id BIGINT UNSIGNED NOT NULL,
  total      INT          NOT NULL DEFAULT 0,
  available  INT          NOT NULL DEFAULT 0,
  reserved   INT          NOT NULL DEFAULT 0,
  version    BIGINT       NOT NULL DEFAULT 0 COMMENT '乐观锁版本号，每次变动 +1',
  updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_stocks_product (product_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS stock_reservations (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  order_no   VARCHAR(40)  NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  quantity   INT          NOT NULL DEFAULT 0,
  status     TINYINT      NOT NULL DEFAULT 1 COMMENT '1 预占中 / 2 已确认 / 3 已释放',
  created_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  -- (order_no, product_id) 必须是**唯一键**而不是普通索引：它是预占幂等的
  -- 事实来源。同一单重放时 repository.ReserveTx 先查同键流水，命中即按幂等
  -- 成功返回（不重复扣）；并发下两侧都先查 miss 时，由本唯一键的冲突裁决，
  -- 第二写入方回查到已有行后同样按重放处理 —— 普通索引挡不住后一种。
  UNIQUE KEY uk_sr_order_product (order_no, product_id),
  KEY idx_sr_product_status (product_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS stock_logs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  product_id  BIGINT UNSIGNED NOT NULL,
  delta       INT          NOT NULL DEFAULT 0 COMMENT '正为入库/释放，负为预占/出库',
  reason      VARCHAR(128) NOT NULL DEFAULT '',
  operator_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_slogs_product (product_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS outbox (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  topic       VARCHAR(128) NOT NULL,
  event_type  VARCHAR(64)  NOT NULL,
  payload     TEXT,
  traceparent VARCHAR(255) NOT NULL DEFAULT '',
  status      TINYINT      NOT NULL DEFAULT 0,
  retry_count INT          NOT NULL DEFAULT 0,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_outbox_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------- 积分域 ----------------------------
CREATE DATABASE IF NOT EXISTS points_db DEFAULT CHARSET utf8mb4;
USE points_db;

-- 版本账：本库自己的迁移记录（一域一库 ⇒ 一库一份，不跨域共用）
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    VARCHAR(64) NOT NULL PRIMARY KEY,
  applied_at TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='本库已执行的建表脚本版本';
INSERT IGNORE INTO schema_migrations (version) VALUES ('2026.10.07-1');

CREATE TABLE IF NOT EXISTS point_accounts (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id      BIGINT UNSIGNED NOT NULL,
  balance      BIGINT       NOT NULL DEFAULT 0,
  total_earned BIGINT       NOT NULL DEFAULT 0,
  total_spent  BIGINT       NOT NULL DEFAULT 0,
  updated_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_pa_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- uk_order_no_type 是幂等的事实来源：kvx 那层只是快速失败，
-- 键丢了也不能重复记账，最终由这唯一键裁决。
CREATE TABLE IF NOT EXISTS point_ledgers (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  user_id       BIGINT UNSIGNED NOT NULL,
  order_no      VARCHAR(40)  NOT NULL,
  type          TINYINT      NOT NULL DEFAULT 1 COMMENT '1 入账 / 2 消费',
  amount        BIGINT       NOT NULL DEFAULT 0,
  balance_after BIGINT       NOT NULL DEFAULT 0,
  remark        VARCHAR(128) NOT NULL DEFAULT '',
  created_at    DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  UNIQUE KEY uk_ledger_order_type (order_no, type),
  KEY idx_ledger_user (user_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS outbox (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  topic       VARCHAR(128) NOT NULL,
  event_type  VARCHAR(64)  NOT NULL,
  payload     TEXT,
  traceparent VARCHAR(255) NOT NULL DEFAULT '',
  status      TINYINT      NOT NULL DEFAULT 0,
  retry_count INT          NOT NULL DEFAULT 0,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_outbox_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------- 种子数据（可选） ----------------------------
USE product_db;
INSERT IGNORE INTO categories (id, name, parent_id) VALUES (1, '生鲜', 0), (2, '数码', 0), (3, '图书', 0);
-- 商品种子与 stock_db.stocks 的 product_id 对齐（1 号有库存），否则 GetProduct
-- 恒 not found —— 演示链路第一步就断在数据上，而不是任何组件上。
INSERT IGNORE INTO products (id, title, price, status, category_id) VALUES
  (1, '高山娃娃菜（2颗装）', 990,  1, 1),
  (2, '赣南脐橙（5斤）',     3990, 1, 1);

USE stock_db;
INSERT IGNORE INTO stocks (product_id, total, available, reserved) VALUES (1, 1000, 1000, 0);
