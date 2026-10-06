# wire 代码生成说明

`wire_gen.go` 是**生成产物，MUST NOT 手工编辑**。改 provider 或注入器后按本文件
重新生成，并把生成物一并提交入库。

## 固定版本

| 项 | 值 | 为什么固定 |
|---|---|---|
| 库 | `github.com/goforj/wire v1.2.0` | 不同版本产出的格式可能不同，不固定会造成 diff 噪声（W7） |
| CLI 安装 | `go install github.com/goforj/wire/cmd/wire@v1.2.0` | **不要用 `@latest`** |
| Go | ≥ 1.25（本仓 `go 1.27.1`） | goforj/wire 的硬要求 |

## 重新生成

```bash
# CLI 装到临时目录，避免污染 $GOPATH/bin
GOBIN=/tmp/wirebin go install github.com/goforj/wire/cmd/wire@v1.2.0

# 在仓根执行（wire CLI 需要 go 在 PATH 里；本机 go 不在默认 PATH）
cd /Users/hyx/work/gowork/src/hubx/launchx
PATH="$HOME/sdk/go1.24.3/bin:$PATH" /tmp/wirebin/wire gen ./internal/wiring/
```

生成后应看到 `✓ wire: ... wrote .../wire_gen.go`，且重复执行两次产物
**逐字节一致**（已验证：两次 shasum 相同）。

## 两条必须记住的约束

1. **`wire.go`（`//go:build wireinject`）里只放注入器。**
   本 fork（v1.2.0）的生成器会把 wireinject 文件的**全部声明原样拷进**
   `wire_gen.go`，包括 `var X = wire.NewSet(...)` 这类变量。拷进去之后
   `wire_gen.go` 就会 import wire，运行时不再零依赖 —— W1 当场失效。
   这是实测踩到的：第一版把 ProviderSet 写在 `wire.go`，生成物第 10 行就是
   `import "github.com/goforj/wire"`。provider 现在直接列在 `wire.Build(...)` 里，
   ProviderSet 的分组语义由 `wiring.go` 的 `Catalog` 承载。

2. **判据不能用 `go list -deps` 直接断言。**
   Go 解析包依赖时会把**所有**构建配置（含被 build tag 排除的文件）一并计入，
   所以模块内只要有一处 import wire，依赖闭包里就一定出现它 —— 这不代表运行时
   真的链了 wire。真正能区分二者的证据是链接产物的模块清单：
   `go test -c -o probe.test ./internal/wiring/` 之后
   `go version -m probe.test | grep wire`（应为空）。
   该断言已固化在 `wiring_test.go:TestW1WireIsBuildTimeOnly` 的第三段。
