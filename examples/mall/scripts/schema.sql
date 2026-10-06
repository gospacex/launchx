-- ============================================================
-- mall 示例 · 四域私有库建表脚本
--
-- 纪律：一域一库（product_db / order_db / stock_db / points_db），
-- 跨域只存 ID，不建跨库外键 —— 外键会把"服务边界"变成"数据库耦合"。
--
-- 用法（本机 compose 里的 MySQL，账号 mall_rw）：
--   mysql -h127.0.0.1 -P3306 -uroot -p < examples/mall/scripts/schema.sql
-- ============================================================

-- ---------------------------- 商品域 ----------------------------
CREATE DATABASE IF NOT EXISTS product_db DEFAULT CHARSET utf8mb4;
USE product_db;

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
  status      TINYINT      NOT NULL DEFAULT 0 COMMENT '0 pending / 1 sent / 2 abandon',
  retry_count INT          NOT NULL DEFAULT 0,
  created_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at  DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (id),
  KEY idx_outbox_status (status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- ---------------------------- 订单域 ----------------------------
CREATE DATABASE IF NOT EXISTS order_db DEFAULT CHARSET utf8mb4;
USE order_db;

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
  KEY idx_sr_order_product (order_no, product_id),
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

USE stock_db;
INSERT IGNORE INTO stocks (product_id, total, available, reserved) VALUES (1, 1000, 1000, 0);
