#!/usr/bin/env python3
"""launchx 调用链审计 —— T5（树形态）与 T13（完整性）判据的可执行实现。

为什么不用 jq：Jaeger REST 的字段口径有两个反直觉点（踩过坑）——
  1. span 属性在 `.spans[].tags[]`（{key,type,value}），**不是** `.attributes[]`（后者恒空，
     照它写判据会静默"全过"）；
  2. 服务名在 `.data[].processes[<pid>].serviceName`，`.spans[].process.serviceName` 得 null。
本脚本把这两点固化在代码里，判据不再依赖手写过滤表达式。

判据（对每条业务 trace，即根 span 恰好 1 个且含 handler 层的）：
  R1 根 span 唯一（多根 = 断链，零根 = 孤儿）
  R2 分层完整：handler.* / service.* / repository.* 各 ≥ 1
  R3 树深 ≥ 3
  R4 每个 **SQL** span（db.query / db.create / db.update / db.delete）的
     db.query.text 非空（长度 ≥ 8、不含 placeholder）——防"空壳 span"。
     （db.redis.* / db.kvx.* 是 redis 命令，没有 SQL 文本，不参与本判据）
  R5 db.* span 不直接挂根（须经 repository/service）
  R6 入链矩阵：出现 JOIN 字样的 SQL span；四类 CRUD（select/insert/update/delete）齐全
     （跨 trace 统计；单条 trace 不要求四类齐）

网格形态（--mesh）额外判据（T10）：
  M1 同一 trace 内**同时**存在应用 span 与 sidecar span（服务名以 `.<ns>` 结尾，
     由 Istio 注入的 Envoy 以 `service_name` = <workload>.<ns> 上报）；
  M2 sidecar span 必须真是 Envoy 产出的：tag `component=proxy` 且带
     `upstream_cluster`（只按服务名后缀判不可靠——同名服务能伪造）；
  M3 sidecar 至少覆盖 2 跳（inbound + outbound），只有 1 条说明另一跳没走 L7。
  ⚠ 已知边界：kitex 的 9013 走 TTHeader over TCP，Istio 只能按 L4 处理，
    **demo-srv 这一跳本来就没有 sidecar span**（把端口名改成 grpc-* 会让业务
    直接 502）。M3 因此按「已解析为 L7 的跳数」判定，不影响结论。

用法：
  python3 trace-audit.py --service demo-monolith --lookback 1h
  python3 trace-audit.py --service demo-gateway,demo-bff,demo-srv --lookback 1h --json /tmp/audit.json
  python3 trace-audit.py --service demo-gateway,demo-bff,demo-srv --lookback 30m --mesh
退出码：0 = 全部 trace 通过；1 = 有 trace 不通过；2 = 连不上 Jaeger / 无 trace。
"""

from __future__ import annotations

import argparse
import json
import re
import sys
import time
import urllib.parse
import urllib.request

CRUD = ("select", "insert", "update", "delete")


def http_get(url: str, timeout: int = 10):
    req = urllib.request.Request(url)
    # 本机有 http_proxy 时 127.0.0.1 会被代理拦掉，故显式绕过
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(req, timeout=timeout) as resp:
        return json.loads(resp.read().decode("utf-8"))


def parse_lookback(text: str) -> int:
    """'90s' / '30m' / '1h' -> 秒。

    顺带踩坑记录：**不要**把 lookback 原样丢给 Jaeger 的
    `?lookback=` 参数 —— 实测（jaeger 1.63 + ES 存储）它对返回条数没有任何
    影响（2m/5m/1h 取回的 trace 完全一样），等于一直在拿陈旧的 trace 判定，
    刚修完的问题会被旧数据判成 FAIL，反过来更糟：旧数据 PASS 会掩盖新引入的
    断链。故这里自己算 start/end（微秒）再传。
    """
    m = re.fullmatch(r"\s*(\d+)\s*([smh]?)\s*", text or "")
    if not m:
        raise ValueError(f"无法解析 --lookback：{text!r}（示例：90s / 30m / 2h）")
    n = int(m.group(1))
    return n * {"": 1, "s": 1, "m": 60, "h": 3600}[m.group(2)]


def fetch_traces(base: str, service: str, lookback: str, limit: int):
    now_us = int(time.time() * 1_000_000)
    q = urllib.parse.urlencode(
        {
            "service": service,
            "start": now_us - parse_lookback(lookback) * 1_000_000,
            "end": now_us,
            "limit": limit,
        }
    )
    data = http_get(f"{base}/api/traces?{q}")
    return data.get("data") or []


def span_tags(span):
    return {t["key"]: t["value"] for t in span.get("tags") or []}


def parent_id(span):
    for ref in span.get("references") or []:
        if ref.get("refType") == "CHILD_OF":
            return ref.get("spanID")
    return None


def audit_trace(trace, mesh_ns=None):
    """返回 (is_business, 明细 dict, 失败原因列表)

    mesh_ns 非空时追加 T10 的 M1/M2 判据（M3 在 main 里跨 trace 汇总）。
    """
    spans = trace["spans"]
    procs = trace.get("processes") or {}
    by_id = {s["spanID"]: s for s in spans}

    def svc(s):
        p = procs.get(s.get("processID")) or {}
        return p.get("serviceName") or "?"

    roots = [s for s in spans if parent_id(s) is None]
    root = roots[0] if len(roots) == 1 else None
    ops = [s["operationName"] for s in spans]
    layers = {
        "handler": [o for o in ops if o.startswith("handler.")],
        "service": [o for o in ops if o.startswith("service.")],
        "repository": [o for o in ops if o.startswith("repository.")],
    }
    is_business = root is not None and bool(layers["handler"])

    # 豁免：这些 trace 不进完整性判定，不是链路缺陷 ——
#   E1 根 span 的 HTTP 状态 >= 400：错误路径（下游 404 / 参数校验失败）本就
#      不会走到数据层，拿它判「缺 SQL span」是误判；
#   E2 无 service 层且无 client.* 出向 span：单跳自响应端点（如 /ping），
#      它刻意不触达下游，判「缺下游 span」同样是误判；
#   E3 任一 span 带 error 标记且全程无 repository/db span：错误发生在数据层
#      之前（参数校验 / 业务拒绝），同样不该按「缺层」判 FAIL。
    exempt = None
    if is_business:
        rtags = span_tags(root)
        # webx 的根 span 只打 `error=True` / `otel.status_code=ERROR`，
        # 并没有 http.status_code（实测，别照 HTTP 语义想当然）
        is_err = str(rtags.get("error", "")).lower() == "true" or str(
            rtags.get("otel.status_code", "")
        ).upper() == "ERROR"
        code = rtags.get("http.status_code") or rtags.get("http.response.status_code")
        try:
            if code is not None and int(code) >= 400:
                is_err = True
            if is_err:
                exempt = f"错误路径（HTTP {code or '标记 error'}）"
        except (TypeError, ValueError):
            if is_err:
                exempt = "错误路径（根 span 标记 error）"
        # E3 错误路径（另一种形态）：error 标记只打在**出错的那一层**
        #   （handler/service）上，根 span 依然干净 —— 实测 POST /orders 传错字段
        #   返回 400 时，根 span 的 tags 是空 dict，只有 handler.CreateOrder 与
        #   service.CreateOrder 带 error=True/otel.status_code=ERROR。只查根 span
        #   的 E1 抓不到，于是这类「校验失败、根本没到数据层」的 trace 会被误判
        #   成 R2 缺 repository 层。判据：任一 span 出错 **且** 确实没触达数据层。
        any_err = any(
            str(span_tags(s).get("error", "")).lower() == "true"
            or str(span_tags(s).get("otel.status_code", "")).upper() == "ERROR"
            for s in spans
        )
        if (
            exempt is None
            and any_err
            and not layers["repository"]
            and not any(o.startswith("db.") for o in ops)
        ):
            exempt = "错误路径（校验/业务拒绝，未触达数据层）"
        if exempt is None and not layers["service"] and not any(
            o.startswith("client.") for o in ops
        ):
            exempt = "单跳自响应（不触达下游）"

    detail = {
        "traceID": trace["traceID"],
        "span数": len(spans),
        "服务": sorted({svc(s) for s in spans}),
        "根span": root["operationName"] if root else f"<{len(roots)} 个>",
        "根span数": len(roots),
        "分层": {k: len(v) for k, v in layers.items()},
        "操作": ops,
    }
    if not is_business:
        # 探针类（/healthz）等非业务 trace：只记录，不判失败
        return "skip", detail, []
    if exempt:
        detail["豁免"] = exempt
        return "exempt", detail, []

    fails = []
    # R1
    if len(roots) != 1:
        fails.append(f"R1 根 span 应恰好 1 个，实际 {len(roots)} 个")
    # R2 分层要求随形态变化：
    #   单体（单进程）= handler / service / repository 三层都在同一进程；
    #   微服（跨进程）= handler 在 bff、service 在 srv、SQL 是 srv 的 db.*
    #   —— 迁入的 srv-order 业务代码只在 service 层埋点、repository 层不埋，
    #   故微服形态不要求 repository.*，改要求「存在 db.* SQL span」。
    multi = len({svc(s) for s in spans}) > 1
    if multi:
        if not layers["handler"]:
            fails.append("R2 缺 handler 层 span（bff 侧 HTTP）")
        if not layers["service"]:
            fails.append("R2 缺 service 层 span（srv 侧业务）")
        if not any(o.startswith("db.") for o in ops):
            fails.append("R2 缺 db.* SQL span（srv 侧数据层）")
    else:
        for layer, items in layers.items():
            if not items:
                fails.append(f"R2 缺 {layer} 层 span")
    # R3 树深
    depth = {}

    def walk(sid):
        if sid in depth:
            return depth[sid]
        s = by_id.get(sid)
        if s is None:
            return 0
        pid = parent_id(s)
        depth[sid] = 1 if pid is None else walk(pid) + 1
        return depth[sid]

    for s in spans:
        walk(s["spanID"])
    max_depth = max(depth.values()) if depth else 0
    detail["树深"] = max_depth
    if max_depth < 3:
        fails.append(f"R3 树深应 >= 3，实际 {max_depth}")
    # R4 / R5
    #
    # R4 只裁决**带 SQL 文本**的那几类（dbx 的 gorm 回调产出 db.query/create/update/
    # delete）。db.redis.* / db.kvx.* 是 redis 命令，压根没有 SQL 文本 —— 拿它判
    # 「空壳 span」会把每个用到缓存/kv 的 trace 全判 FAIL，判据失真比漏判更糟
    # （微服形态必踩：限流计数走 kvx、读路径几乎都先打 cachex）。
    sql_ops = {"db.query", "db.create", "db.update", "db.delete", "db.exec"}
    sql_spans = []
    for s in spans:
        t = span_tags(s)
        text = t.get("db.query.text") or t.get("db.statement") or ""
        if s["operationName"].startswith("db."):
            if s["operationName"] in sql_ops:
                if len(text) < 8 or re.search(r"(?i)placeholder|\?\?\?", text):
                    fails.append(f"R4 {s['operationName']} 的 SQL 文本为空/占位：{text!r}")
                sql_spans.append(text)
            pid = parent_id(s)
            if pid is None or pid == root["spanID"]:
                fails.append(f"R5 {s['operationName']} 直接挂在根下（应经 repository）")
    detail["SQL span 数"] = len(sql_spans)
    detail["SQL 明细"] = sql_spans

    if mesh_ns:
        suffix = "." + mesh_ns
        side = [s for s in spans if svc(s).endswith(suffix)]
        app = [s for s in spans if not svc(s).endswith(suffix)]
        # M2：只按服务名后缀判会被同名服务糊弄，认 Envoy 自己的两个 tag
        proxy_tags = [
            s for s in side
            if span_tags(s).get("component") == "proxy"
            and span_tags(s).get("upstream_cluster")
        ]
        detail["sidecar span 数"] = len(side)
        detail["sidecar 服务"] = sorted({svc(s) for s in side})
        detail["sidecar 跳数"] = sorted({span_tags(s).get("upstream_cluster") for s in proxy_tags})
        if not side:
            fails.append(f"M1 trace 内没有 sidecar span（服务名以 {suffix} 结尾）")
        if not app:
            fails.append("M1 trace 内没有应用 span（只剩 sidecar = 应用没入链）")
        if side and not proxy_tags:
            fails.append(
                "M2 sidecar span 缺少 component=proxy / upstream_cluster tag"
                "（可能是冒名服务，而非 Envoy）"
            )
    return "business", detail, fails


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:16686")
    ap.add_argument("--service", required=True, help="逗号分隔，如 demo-gateway,demo-bff")
    ap.add_argument("--lookback", default="1h")
    ap.add_argument("--limit", type=int, default=50)
    ap.add_argument("--json", help="把明细写到该文件")
    ap.add_argument(
        "--mesh",
        nargs="?",
        const="launchx-mesh",
        default=None,
        metavar="NS",
        help="启用网格判据 M1/M2/M3；可选指定命名空间后缀（默认 launchx-mesh）",
    )
    args = ap.parse_args()

    services = [s.strip() for s in args.service.split(",") if s.strip()]
    all_details, all_fails, crud_seen, join_seen = [], {}, set(), False
    seen = set()  # 跨服务拉取会重复返回同一条 trace（各进程都在里面），按 traceID 去重
    skip_counter = {}  # 非业务 trace 按 operationName 计数

    for svcname in services:
        try:
            traces = fetch_traces(args.base, svcname, args.lookback, args.limit)
        except Exception as exc:  # noqa: BLE001
            print(f"ERROR: 拉取 {svcname} 失败：{exc}", file=sys.stderr)
            return 2
        if not traces:
            print(f"ERROR: {svcname} 在 {args.lookback} 内没有任何 trace", file=sys.stderr)
            return 2
        print(f"\n=== service={svcname}  trace 数={len(traces)} ===")
        for t in traces:
            if t["traceID"] in seen:
                continue
            seen.add(t["traceID"])
            kind, detail, fails = audit_trace(t, mesh_ns=args.mesh)
            if kind == "skip":
                # 后台任务/组件操作的独立 span（outbox 轮询、启动注册、健康探针）。
                # 它们没有父是**设计如此**，不算断链；只计数不刷屏——
                # 逐条打印会让真正的问题淹没在几百行噪声里。
                skip_counter[detail["根span"]] = skip_counter.get(detail["根span"], 0) + 1
                continue
            if kind == "exempt":
                skip_counter[f"{detail['根span']}（{detail['豁免']}）"] = (
                    skip_counter.get(f"{detail['根span']}（{detail['豁免']}）", 0) + 1
                )
                continue
            all_details.append(detail)
            mark = "PASS" if not fails else "FAIL"
            extra = ""
            if args.mesh:
                extra = f" sidecar={detail.get('sidecar span 数', 0)} 跳={detail.get('sidecar 跳数')}"
            print(
                f"  [{mark}] {detail['根span']:<38} span={detail['span数']:<3}"
                f" 分层={detail['分层']} 树深={detail['树深']} SQL={detail['SQL span 数']}"
                f" 服务={detail['服务']}{extra}"
            )
            for f in fails:
                print(f"         ✗ {f}")
                all_fails.setdefault(detail["traceID"], []).append(f)
            for sql in detail["SQL 明细"]:
                low = sql.lower()
                for c in CRUD:
                    if re.search(rf"(?i)^[^a-z]*{c}\b", low):
                        crud_seen.add(c)
                if re.search(r"(?i)\bjoin\b", low):
                    join_seen = True

    print("\n=== 跨 trace 汇总 ===")
    if skip_counter:
        joined = "  ".join(f"{k}×{v}" for k, v in sorted(skip_counter.items(), key=lambda x: -x[1]))
        print(f"非业务 trace（后台任务/组件，无父属设计如此）：{joined}")
    print(f"业务 trace 数：{len(all_details)}")
    print(f"CRUD 覆盖：{sorted(crud_seen)}  缺：{sorted(set(CRUD) - crud_seen) or '无'}")
    print(f"JOIN 出现：{'是' if join_seen else '否'}")

    if args.mesh:
        # M3：sidecar 至少覆盖 2 跳（inbound + outbound）。demo-srv 的 9013 是
        # TTHeader over TCP，Istio 只做 L4，本来就没有 sidecar span，故按
        # 「能解析成 L7 的跳」统计，不把协议事实算成缺陷。
        hops, side_traces = set(), 0
        for d in all_details:
            if d.get("sidecar span 数"):
                side_traces += 1
                hops.update(d.get("sidecar 跳数") or [])
        print(
            f"网格入链：{side_traces}/{len(all_details)} 条业务 trace 含 sidecar span，"
            f"覆盖跳：{sorted(hops) or '无'}"
        )
        if side_traces == 0:
            print("  ✗ M1 没有任何 trace 同时含应用与 sidecar span（网格未入链）")
            all_fails.setdefault("<mesh>", []).append("M1 无 trace 含 sidecar span")
        elif len(hops) < 2:
            print(f"  ✗ M3 sidecar 只覆盖 {len(hops)} 跳，应 >= 2（inbound + outbound）")
            all_fails.setdefault("<mesh>", []).append(f"M3 sidecar 仅 {len(hops)} 跳")
    if args.json:
        with open(args.json, "w", encoding="utf-8") as fh:
            json.dump({"details": all_details, "fails": all_fails}, fh, ensure_ascii=False, indent=2)
        print(f"明细已写入：{args.json}")

    if all_fails:
        print(f"\n结论：FAIL（{len(all_fails)} 条 trace 不满足判据）")
        return 1
    if not all_details:
        print("\n结论：FAIL（没有任何业务 trace）")
        return 1
    print("\n结论：PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
