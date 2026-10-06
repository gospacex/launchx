#!/usr/bin/env python3
"""生成 examples/mall 四个微服的 kitex/prutal RPC 桩。

为什么不用 kitex CLI：本机没有 kitex / thriftgo / protoc（实测 which 全为空），
而桩代码是**完全模式化**的（每个方法 = handler + Args + Result + kClient 方法四段），
故改为从域契约生成，契约改了重跑本脚本即可，产物与 kitex v0.16.2 生成物同构。

用法：
    python3 examples/mall/scripts/gen_kitex_stub.py

产物（每域 4 个文件）：
    pkg/kitex_gen/<domain>/<domain>.pb.go        消息体 + <Svc> 接口
    pkg/kitex_gen/<domain>/<domain>/client.go    客户端
    pkg/kitex_gen/<domain>/<domain>/server.go    服务端
    pkg/kitex_gen/<domain>/<domain>/<domain>.go  serviceInfo + handler + codec
"""

import os

PKG = "github.com/gospacex/launchx/examples/mall/pkg/kitex_gen"
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "pkg", "kitex_gen")
KITEX_VER = "v0.16.2"

# 字段：(Go 字段名, Go 类型, protobuf 线型, json 名)
# 线型只取 varint / bytes：金额一律 int64（分），时间一律 int64（Unix 秒），
# 不引 float —— 浮点金额跨服务往返会有精度差。
F = lambda n, t, w, j: (n, t, w, j)


def msg(name, fields):
    return {"name": name, "fields": fields}


DOMAINS = [
    {
        # ---------------- 商品域 ----------------
        "domain": "product",
        "svc": "Product",
        "msgs": [
            msg("ProductInfo", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Title", "string", "bytes", "title"),
                F("Price", "int64", "varint", "price"),          # 分
                F("Status", "int32", "varint", "status"),        # 1 上架 / 2 下架
                F("CategoryId", "int64", "varint", "category_id"),
                F("CategoryName", "string", "bytes", "category_name"),
                F("SkuCount", "int32", "varint", "sku_count"),
                F("CreatedAt", "int64", "varint", "created_at"),
            ]),
            msg("CreateProductReq", [
                F("OperatorId", "int64", "varint", "operator_id"),
                F("Title", "string", "bytes", "title"),
                F("Price", "int64", "varint", "price"),
                F("CategoryId", "int64", "varint", "category_id"),
                F("InitStock", "int32", "varint", "init_stock"),
            ]),
            msg("CreateProductResp", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("GetProductReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("ProductId", "int64", "varint", "product_id"),
            ]),
            msg("GetProductResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Product", "*ProductInfo", "bytes", "product"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("UpdateProductReq", [
                F("OperatorId", "int64", "varint", "operator_id"),
                F("ProductId", "int64", "varint", "product_id"),
                F("Title", "string", "bytes", "title"),
                F("Price", "int64", "varint", "price"),
                F("CategoryId", "int64", "varint", "category_id"),
            ]),
            msg("UpdateProductResp", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("UpdateStatusReq", [
                F("OperatorId", "int64", "varint", "operator_id"),
                F("ProductId", "int64", "varint", "product_id"),
                F("Status", "int32", "varint", "status"),
            ]),
            msg("UpdateStatusResp", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Status", "int32", "varint", "status"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ListProductsReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("CategoryId", "int64", "varint", "category_id"),
                F("Status", "int32", "varint", "status"),
                F("Limit", "int32", "varint", "limit"),
                F("Offset", "int32", "varint", "offset"),
            ]),
            msg("ListProductsResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Products", "[]*ProductInfo", "bytes", "products"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ProductReportRow", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Title", "string", "bytes", "title"),
                F("CategoryName", "string", "bytes", "category_name"),
                F("Price", "int64", "varint", "price"),
                F("SkuCount", "int32", "varint", "sku_count"),
                F("OnSaleSkus", "int32", "varint", "on_sale_skus"),
                F("SoldCount", "int32", "varint", "sold_count"),
            ]),
            msg("ReportReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("CategoryId", "int64", "varint", "category_id"),
                F("Limit", "int32", "varint", "limit"),
            ]),
            msg("ReportResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Rows", "[]*ProductReportRow", "bytes", "rows"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
        ],
        "methods": [
            ("CreateProduct", "CreateProductReq", "CreateProductResp"),
            ("GetProduct", "GetProductReq", "GetProductResp"),
            ("UpdateProduct", "UpdateProductReq", "UpdateProductResp"),
            ("UpdateStatus", "UpdateStatusReq", "UpdateStatusResp"),
            ("ListProducts", "ListProductsReq", "ListProductsResp"),
            ("Report", "ReportReq", "ReportResp"),
        ],
    },
    {
        # ---------------- 订单域 ----------------
        "domain": "order",
        "svc": "Order",
        "msgs": [
            msg("OrderItem", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Name", "string", "bytes", "name"),
                F("Quantity", "int32", "varint", "quantity"),
                F("UnitPrice", "int64", "varint", "unit_price"),
            ]),
            msg("CreateOrderReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("Items", "[]*OrderItem", "bytes", "items"),
            ]),
            msg("CreateOrderResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("OrderId", "int64", "varint", "order_id"),
                F("OrderNo", "string", "bytes", "order_no"),
                F("TotalAmount", "int64", "varint", "total_amount"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("GetOrderReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("OrderId", "int64", "varint", "order_id"),
            ]),
            msg("GetOrderResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("OrderId", "int64", "varint", "order_id"),
                F("OrderNo", "string", "bytes", "order_no"),
                F("Status", "string", "bytes", "status"),
                F("TotalAmount", "int64", "varint", "total_amount"),
                F("PaidAmount", "int64", "varint", "paid_amount"),
                F("Items", "[]*OrderItem", "bytes", "items"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ListOrdersReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("FromDate", "string", "bytes", "from_date"),
                F("ToDate", "string", "bytes", "to_date"),
                F("Limit", "int32", "varint", "limit"),
            ]),
            msg("OrderFlow", [
                F("Id", "int64", "varint", "id"),
                F("UserId", "int64", "varint", "user_id"),
                F("OrderNo", "string", "bytes", "order_no"),
                F("Status", "string", "bytes", "status"),
                F("TotalAmount", "int64", "varint", "total_amount"),
                F("CreatedAt", "int64", "varint", "created_at"),
            ]),
            msg("ListOrdersResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Flows", "[]*OrderFlow", "bytes", "flows"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ReportReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("FromDate", "string", "bytes", "from_date"),
                F("ToDate", "string", "bytes", "to_date"),
            ]),
            msg("ReportRow", [
                F("OrderNo", "string", "bytes", "order_no"),
                F("Status", "string", "bytes", "status"),
                F("TotalAmount", "int64", "varint", "total_amount"),
                F("RefundAmount", "int64", "varint", "refund_amount"),
                F("ItemCount", "int32", "varint", "item_count"),
                F("StatusChangeCount", "int32", "varint", "status_change_count"),
            ]),
            msg("ReportResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Rows", "[]*ReportRow", "bytes", "rows"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
        ],
        "methods": [
            ("CreateOrder", "CreateOrderReq", "CreateOrderResp"),
            ("GetOrder", "GetOrderReq", "GetOrderResp"),
            ("ListOrders", "ListOrdersReq", "ListOrdersResp"),
            ("Report", "ReportReq", "ReportResp"),
        ],
    },
    {
        # ---------------- 库存域 ----------------
        "domain": "stock",
        "svc": "Stock",
        "msgs": [
            msg("StockInfo", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Total", "int32", "varint", "total"),
                F("Available", "int32", "varint", "available"),
                F("Reserved", "int32", "varint", "reserved"),
                F("Version", "int64", "varint", "version"),
            ]),
            msg("GetStockReq", [
                F("ProductId", "int64", "varint", "product_id"),
            ]),
            msg("GetStockResp", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Stock", "*StockInfo", "bytes", "stock"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ListStocksReq", [
                F("Limit", "int32", "varint", "limit"),
                F("Offset", "int32", "varint", "offset"),
            ]),
            msg("ListStocksResp", [
                F("Stocks", "[]*StockInfo", "bytes", "stocks"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ReserveStockReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("OrderNo", "string", "bytes", "order_no"),
                F("ProductId", "int64", "varint", "product_id"),
                F("Quantity", "int32", "varint", "quantity"),
            ]),
            msg("ReserveStockResp", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Reserved", "int32", "varint", "reserved"),
                F("ReservationId", "int64", "varint", "reservation_id"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ReleaseStockReq", [
                F("OrderNo", "string", "bytes", "order_no"),
                F("ProductId", "int64", "varint", "product_id"),
                F("Quantity", "int32", "varint", "quantity"),
            ]),
            msg("ReleaseStockResp", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Available", "int32", "varint", "available"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ConfirmStockReq", [
                F("OrderNo", "string", "bytes", "order_no"),
                F("ProductId", "int64", "varint", "product_id"),
                F("Quantity", "int32", "varint", "quantity"),
            ]),
            msg("ConfirmStockResp", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Total", "int32", "varint", "total"),
                F("Available", "int32", "varint", "available"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("AdjustStockReq", [
                F("OperatorId", "int64", "varint", "operator_id"),
                F("ProductId", "int64", "varint", "product_id"),
                F("Delta", "int32", "varint", "delta"),
                F("Reason", "string", "bytes", "reason"),
            ]),
            msg("AdjustStockResp", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Total", "int32", "varint", "total"),
                F("Available", "int32", "varint", "available"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("StockReportRow", [
                F("ProductId", "int64", "varint", "product_id"),
                F("Total", "int32", "varint", "total"),
                F("Available", "int32", "varint", "available"),
                F("Reserved", "int32", "varint", "reserved"),
                F("ReservationCount", "int32", "varint", "reservation_count"),
                F("LogCount", "int32", "varint", "log_count"),
            ]),
            msg("ReportReq", [
                F("Limit", "int32", "varint", "limit"),
            ]),
            msg("ReportResp", [
                F("Rows", "[]*StockReportRow", "bytes", "rows"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
        ],
        "methods": [
            ("GetStock", "GetStockReq", "GetStockResp"),
            ("ListStocks", "ListStocksReq", "ListStocksResp"),
            ("ReserveStock", "ReserveStockReq", "ReserveStockResp"),
            ("ReleaseStock", "ReleaseStockReq", "ReleaseStockResp"),
            ("ConfirmStock", "ConfirmStockReq", "ConfirmStockResp"),
            ("AdjustStock", "AdjustStockReq", "AdjustStockResp"),
            ("Report", "ReportReq", "ReportResp"),
        ],
    },
    {
        # ---------------- 积分域 ----------------
        "domain": "points",
        "svc": "Points",
        "msgs": [
            msg("PointAccount", [
                F("UserId", "int64", "varint", "user_id"),
                F("Balance", "int64", "varint", "balance"),
                F("TotalEarned", "int64", "varint", "total_earned"),
                F("TotalSpent", "int64", "varint", "total_spent"),
            ]),
            msg("GetAccountReq", [
                F("UserId", "int64", "varint", "user_id"),
            ]),
            msg("GetAccountResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Account", "*PointAccount", "bytes", "account"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("EarnReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("OrderNo", "string", "bytes", "order_no"),
                F("Amount", "int64", "varint", "amount"),
                F("Remark", "string", "bytes", "remark"),
            ]),
            msg("EarnResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Balance", "int64", "varint", "balance"),
                F("LedgerId", "int64", "varint", "ledger_id"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ConsumeReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("OrderNo", "string", "bytes", "order_no"),
                F("Amount", "int64", "varint", "amount"),
                F("Remark", "string", "bytes", "remark"),
            ]),
            msg("ConsumeResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Balance", "int64", "varint", "balance"),
                F("LedgerId", "int64", "varint", "ledger_id"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ListLedgersReq", [
                F("UserId", "int64", "varint", "user_id"),
                F("Limit", "int32", "varint", "limit"),
            ]),
            msg("PointLedger", [
                F("Id", "int64", "varint", "id"),
                F("UserId", "int64", "varint", "user_id"),
                F("OrderNo", "string", "bytes", "order_no"),
                F("Type", "int32", "varint", "type"),          # 1 入账 / 2 消费
                F("Amount", "int64", "varint", "amount"),
                F("BalanceAfter", "int64", "varint", "balance_after"),
                F("CreatedAt", "int64", "varint", "created_at"),
            ]),
            msg("ListLedgersResp", [
                F("UserId", "int64", "varint", "user_id"),
                F("Ledgers", "[]*PointLedger", "bytes", "ledgers"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
            msg("ReportReq", [
                F("UserId", "int64", "varint", "user_id"),
            ]),
            msg("ReportRow", [
                F("UserId", "int64", "varint", "user_id"),
                F("Balance", "int64", "varint", "balance"),
                F("TotalEarned", "int64", "varint", "total_earned"),
                F("TotalSpent", "int64", "varint", "total_spent"),
                F("LedgerCount", "int32", "varint", "ledger_count"),
                F("LastChangeAt", "int64", "varint", "last_change_at"),
            ]),
            msg("ReportResp", [
                F("Rows", "[]*ReportRow", "bytes", "rows"),
                F("Code", "int64", "varint", "code"),
                F("Message", "string", "bytes", "message"),
            ]),
        ],
        "methods": [
            ("GetAccount", "GetAccountReq", "GetAccountResp"),
            ("Earn", "EarnReq", "EarnResp"),
            ("Consume", "ConsumeReq", "ConsumeResp"),
            ("ListLedgers", "ListLedgersReq", "ListLedgersResp"),
            ("Report", "ReportReq", "ReportResp"),
        ],
    },
]


def zero(t):
    if t.startswith("[]") or t.startswith("*"):
        return "nil"
    if t in ("int64", "int32", "int"):
        return "0"
    if t == "string":
        return '""'
    if t == "bool":
        return "false"
    raise ValueError("unsupported type: " + t)


def pb_tag(i, gotype, wire):
    idx = i + 1
    kind = "rep" if gotype.startswith("[]") else "opt"
    return 'protobuf:"%s,%d,%s,name=%%s"' % (wire, idx, kind)


def gen_pb(d):
    dm, svc = d["domain"], d["svc"]
    L = ["// Code generated by Kitex %s. DO NOT EDIT." % KITEX_VER, "", "package " + dm, "",
         "import (", '\t"context"', "", '\t"github.com/cloudwego/prutal"', ")"]
    for m in d["msgs"]:
        name = m["name"]
        L += ["", "type %s struct {" % name]
        for i, (fn, gt, wire, js) in enumerate(m["fields"]):
            L.append("\t%s %s `protobuf:\"%s,%d,%s,name=%s\" json:\"%s,omitempty\"`" % (
                fn, gt, wire, i + 1, "rep" if gt.startswith("[]") else "opt", js, js))
        L += ["}", "", "func (x *%s) Reset() { *x = %s{} }" % (name, name), "",
              "func (x *%s) Marshal(in []byte) ([]byte, error) { return prutal.MarshalAppend(in, x) }" % name,
              "",
              "func (x *%s) Unmarshal(in []byte) error { return prutal.Unmarshal(in, x) }" % name]
        for fn, gt, _, _ in m["fields"]:
            L += ["", "func (x *%s) Get%s() %s {" % (name, fn, gt),
                  "\tif x != nil {", "\t\treturn x.%s" % fn, "\t}",
                  "\treturn %s" % zero(gt), "}"]
    L += ["", "type %s interface {" % svc]
    for name, req, resp in d["methods"]:
        L.append("\t%s(ctx context.Context, req *%s) (res *%s, err error)" % (name, req, resp))
    L += ["}", ""]
    return "\n".join(L)


def gen_client(d):
    dm = d["domain"]
    L = ["// Code generated by Kitex %s. DO NOT EDIT." % KITEX_VER, "", "package " + dm, "",
         "import (", '\t"context"', '\tclient "github.com/cloudwego/kitex/client"',
         '\tcallopt "github.com/cloudwego/kitex/client/callopt"',
         '\t%s "%s/%s"' % (dm, PKG, dm), ")", "",
         "// Client is designed to provide IDL-compatible methods with call-option parameter for kitex framework.",
         "type Client interface {"]
    for name, req, resp in d["methods"]:
        L.append("\t%s(ctx context.Context, Req *%s.%s, callOptions ...callopt.Option) (r *%s.%s, err error)"
                 % (name, dm, req, dm, resp))
    L += ["}", "", "// NewClient creates a client for the service defined in IDL.",
          "func NewClient(destService string, opts ...client.Option) (Client, error) {",
          "\tvar options []client.Option", "\toptions = append(options, client.WithDestService(destService))",
          "\toptions = append(options, opts...)", "",
          "\tkc, err := client.NewClient(serviceInfo(), options...)", "\tif err != nil {", "\t\treturn nil, err",
          "\t}", "\treturn &k%sClient{" % d["svc"], "\t\tkClient: newServiceClient(kc),", "\t}, nil", "}", "",
          "// MustNewClient creates a client for the service defined in IDL. It panics if any error occurs.",
          "func MustNewClient(destService string, opts ...client.Option) Client {",
          "\tkc, err := NewClient(destService, opts...)", "\tif err != nil {", "\t\tpanic(err)", "\t}",
          "\treturn kc", "}", "", "type k%sClient struct {" % d["svc"], "\t*kClient", "}"]
    for name, req, resp in d["methods"]:
        L += ["", "func (p *k%sClient) %s(ctx context.Context, Req *%s.%s, callOptions ...callopt.Option) (r *%s.%s, err error) {"
              % (d["svc"], name, dm, req, dm, resp),
              "\tctx = client.NewCtxWithCallOptions(ctx, callOptions)",
              "\treturn p.kClient.%s(ctx, Req)" % name, "}"]
    L.append("")
    return "\n".join(L)


def gen_server(d):
    dm, svc = d["domain"], d["svc"]
    return "\n".join([
        "// Code generated by Kitex %s. DO NOT EDIT." % KITEX_VER, "package " + dm, "",
        "import (", '\tserver "github.com/cloudwego/kitex/server"', '\t%s "%s/%s"' % (dm, PKG, dm), ")", "",
        "// NewServer creates a server.Server with the given handler and options.",
        "func NewServer(handler %s.%s, opts ...server.Option) server.Server {" % (dm, svc),
        "\tvar options []server.Option", "", "\toptions = append(options, opts...)", "",
        "\tsvr := server.NewServer(options...)",
        "\tif err := svr.RegisterService(serviceInfo(), handler); err != nil {", "\t\tpanic(err)", "\t}",
        "\treturn svr", "}", "",
        "func RegisterService(svr server.Server, handler %s.%s, opts ...server.RegisterOption) error {" % (dm, svc),
        "\treturn svr.RegisterService(serviceInfo(), handler, opts...)", "}", "",
    ])


def gen_service(d):
    dm, svc = d["domain"], d["svc"]
    L = ["// Code generated by Kitex %s. DO NOT EDIT." % KITEX_VER, "", "package " + dm, "", "import (",
         '\t"context"', '\t"errors"', '\tclient "github.com/cloudwego/kitex/client"',
         '\tkitex "github.com/cloudwego/kitex/pkg/serviceinfo"',
         '\tstreaming "github.com/cloudwego/kitex/pkg/streaming"',
         '\tproto "github.com/cloudwego/prutal"', '\t%s "%s/%s"' % (dm, PKG, dm), ")", "",
         "var errInvalidMessageType = errors.New(\"invalid message type for service method handler\")", "",
         "var serviceMethods = map[string]kitex.MethodInfo{"]
    for name, _, _ in d["methods"]:
        lo = name[0].lower() + name[1:]
        L += ['\t"%s": kitex.NewMethodInfo(' % name, "\t\t%sHandler," % lo, "\t\tnew%sArgs," % name,
              "\t\tnew%sResult," % name, "\t\tfalse,",
              "\t\tkitex.WithStreamingMode(kitex.StreamingUnary),", "\t),"]
    L += ["}", "", "var (", "\t%sServiceInfo                = NewServiceInfo()" % dm,
          "\t%sServiceInfoForClient       = NewServiceInfoForClient()" % dm,
          "\t%sServiceInfoForStreamClient = NewServiceInfoForStreamClient()" % dm, ")", "",
          "// for server", "func serviceInfo() *kitex.ServiceInfo {", "\treturn %sServiceInfo" % dm, "}", "",
          "// for stream client", "func serviceInfoForStreamClient() *kitex.ServiceInfo {",
          "\treturn %sServiceInfoForStreamClient" % dm, "}", "",
          "// for client", "func serviceInfoForClient() *kitex.ServiceInfo {",
          "\treturn %sServiceInfoForClient" % dm, "}", "",
          "// NewServiceInfo creates a new ServiceInfo containing all methods",
          "func NewServiceInfo() *kitex.ServiceInfo {", "\treturn newServiceInfo(false, true, true)", "}", "",
          "// NewServiceInfo creates a new ServiceInfo containing non-streaming methods",
          "func NewServiceInfoForClient() *kitex.ServiceInfo {", "\treturn newServiceInfo(false, false, true)",
          "}", "func NewServiceInfoForStreamClient() *kitex.ServiceInfo {",
          "\treturn newServiceInfo(true, true, false)", "}", "",
          "func newServiceInfo(hasStreaming bool, keepStreamingMethods bool, keepNonStreamingMethods bool) *kitex.ServiceInfo {",
          '\tserviceName := "%s"' % svc, "\thandlerType := (*%s.%s)(nil)" % (dm, svc),
          "\tmethods := map[string]kitex.MethodInfo{}", "\tfor name, m := range serviceMethods {",
          "\t\tif m.IsStreaming() && !keepStreamingMethods {", "\t\t\tcontinue", "\t\t}",
          "\t\tif !m.IsStreaming() && !keepNonStreamingMethods {", "\t\t\tcontinue", "\t\t}",
          "\t\tmethods[name] = m", "\t}", "\textra := map[string]interface{}{", '\t\t"PackageName": "%s",' % dm,
          "\t}", "\tif hasStreaming {", '\t\textra["streaming"] = hasStreaming', "\t}",
          "\tsvcInfo := &kitex.ServiceInfo{", "\t\tServiceName:     serviceName,",
          "\t\tHandlerType:     handlerType,", "\t\tMethods:         methods,",
          "\t\tPayloadCodec:    kitex.Protobuf,", '\t\tKiteXGenVersion: "%s",' % KITEX_VER,
          "\t\tExtra:           extra,", "\t}", "\treturn svcInfo", "}"]
    for name, req, resp in d["methods"]:
        lo = name[0].lower() + name[1:]
        L += ["", "func %sHandler(ctx context.Context, handler interface{}, arg, result interface{}) error {" % lo,
              "\tswitch s := arg.(type) {", "\tcase *streaming.Args:", "\t\tst := s.Stream",
              "\t\treq := new(%s.%s)" % (dm, req), "\t\tif err := st.RecvMsg(req); err != nil {",
              "\t\t\treturn err", "\t\t}",
              "\t\tresp, err := handler.(%s.%s).%s(ctx, req)" % (dm, svc, name), "\t\tif err != nil {",
              "\t\t\treturn err", "\t\t}", "\t\treturn st.SendMsg(resp)",
              "\tcase *%sArgs:" % name,
              "\t\tsuccess, err := handler.(%s.%s).%s(ctx, s.Req)" % (dm, svc, name), "\t\tif err != nil {",
              "\t\t\treturn err", "\t\t}", "\t\trealResult := result.(*%sResult)" % name,
              "\t\trealResult.Success = success", "\t\treturn nil", "\tdefault:",
              "\t\treturn errInvalidMessageType", "\t}", "}",
              "func new%sArgs() interface{} {" % name, "\treturn &%sArgs{}" % name, "}", "",
              "func new%sResult() interface{} {" % name, "\treturn &%sResult{}" % name, "}", "",
              "type %sArgs struct {" % name, "\tReq *%s.%s" % (dm, req), "}", "",
              "func (p *%sArgs) Marshal(out []byte) ([]byte, error) {" % name, "\tif !p.IsSetReq() {",
              "\t\treturn out, nil", "\t}", "\treturn proto.Marshal(p.Req)", "}", "",
              "func (p *%sArgs) Unmarshal(in []byte) error {" % name, "\tmsg := new(%s.%s)" % (dm, req),
              "\tif err := proto.Unmarshal(in, msg); err != nil {", "\t\treturn err", "\t}", "\tp.Req = msg",
              "\treturn nil", "}", "", "var %sArgs_Req_DEFAULT *%s.%s" % (name, dm, req), "",
              "func (p *%sArgs) GetReq() *%s.%s {" % (name, dm, req), "\tif !p.IsSetReq() {",
              "\t\treturn %sArgs_Req_DEFAULT" % name, "\t}", "\treturn p.Req", "}", "",
              "func (p *%sArgs) IsSetReq() bool {" % name, "\treturn p.Req != nil", "}", "",
              "func (p *%sArgs) GetFirstArgument() interface{} {" % name, "\treturn p.Req", "}", "",
              "type %sResult struct {" % name, "\tSuccess *%s.%s" % (dm, resp), "}", "",
              "var %sResult_Success_DEFAULT *%s.%s" % (name, dm, resp), "",
              "func (p *%sResult) Marshal(out []byte) ([]byte, error) {" % name, "\tif !p.IsSetSuccess() {",
              "\t\treturn out, nil", "\t}", "\treturn proto.Marshal(p.Success)", "}", "",
              "func (p *%sResult) Unmarshal(in []byte) error {" % name, "\tmsg := new(%s.%s)" % (dm, resp),
              "\tif err := proto.Unmarshal(in, msg); err != nil {", "\t\treturn err", "\t}", "\tp.Success = msg",
              "\treturn nil", "}", "", "func (p *%sResult) GetSuccess() *%s.%s {" % (name, dm, resp),
              "\tif !p.IsSetSuccess() {", "\t\treturn %sResult_Success_DEFAULT" % name, "\t}",
              "\treturn p.Success", "}", "", "func (p *%sResult) SetSuccess(x interface{}) {" % name,
              "\tp.Success = x.(*%s.%s)" % (dm, resp), "}", "",
              "func (p *%sResult) IsSetSuccess() bool {" % name, "\treturn p.Success != nil", "}", "",
              "func (p *%sResult) GetResult() interface{} {" % name, "\treturn p.Success", "}"]
    L += ["", "type kClient struct {", "\tc client.Client", "}", "",
          "func newServiceClient(c client.Client) *kClient {", "\treturn &kClient{", "\t\tc: c,", "\t}", "}"]
    for name, req, resp in d["methods"]:
        L += ["", "func (p *kClient) %s(ctx context.Context, Req *%s.%s) (r *%s.%s, err error) {" % (name, dm, req, dm, resp),
              "\tvar _args %sArgs" % name, "\t_args.Req = Req", "\tvar _result %sResult" % name,
              '\tif err = p.c.Call(ctx, "%s", &_args, &_result); err != nil {' % name, "\t\treturn", "\t}",
              "\treturn _result.GetSuccess(), nil", "}"]
    L.append("")
    return "\n".join(L)


def main():
    for d in DOMAINS:
        dm = d["domain"]
        root = os.path.join(OUT, dm)
        os.makedirs(os.path.join(root, dm), exist_ok=True)
        write(os.path.join(root, "%s.pb.go" % dm), gen_pb(d))
        write(os.path.join(root, dm, "client.go"), gen_client(d))
        write(os.path.join(root, dm, "server.go"), gen_server(d))
        write(os.path.join(root, dm, "%s.go" % dm), gen_service(d))
        print("generated %-8s (%d methods)" % (dm, len(d["methods"])))


def write(path, text):
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)


if __name__ == "__main__":
    main()
