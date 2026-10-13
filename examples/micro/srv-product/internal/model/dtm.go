package model

// dtm 分支面（Saga 销量分支）的领域出入参。
//
// 单独成文件而不是并进 product.go：这些类型的调用方是 dtm server 而不是 kitex，
// 走的是 internal/dtm 分支面而非 handler。混在一起会让「哪些方法能被
// POST /product/{id} 调到」这件事看不出来 —— 而分支方法带 barrier，
// 被普通 RPC 面调到就是一个无屏障的写路径。

// 分支方法名（失败注入 fail_inject、span 名、全方法名共用同一套字面量）。
//
// 为什么要显式列举而不是拿方法名反射：fail_inject 靠字符串命中分支，字符串
// 拼错就是「点火失败」—— 演示脚本以为补偿被点着了，实际正向一路跑绿。故这些
// 字面量在分支 handler 与编排侧共用，且有单测钉住（见 dtm 包的
// TestBranchMethodsUseDtmFullMethodNames）。
const (
	DtmAddSalesBranch    = "AddSales"
	DtmReduceSalesBranch = "ReduceSales"
)

// DtmSalesCmd 分支业务体（Saga 正向与补偿共用同一形状）。
//
// 共用是 dtm 的载荷语义决定的，不是偷懒：Saga 的 Add(action, compensate, payload)
// 只有一个 payload，补偿分支收的是**同一份**业务体。拆成两个结构体只会在字段
// 漂移时多出一处要同步的地方，而字段本来就必须一致 —— 不一致等于正向与补偿
// 算的不是同一笔账（少减一分就是一个永久对不上的销量）。
type DtmSalesCmd struct {
	// ProductID 商品维度：销量是商品维度资源，不按用户分片。
	ProductID uint64 `json:"product_id"`
	// Quantity 本次分支要动的销量数（恒为正：方向由方法语义决定，AddSales
	// 是加、ReduceSales 是减，载荷里不传符号，避免「负数加销量」这类歧义）。
	//
	// 必须为正：传 0 或负数会让 ReduceSales 的 `sold >= ?` 守卫恒成立，
	// 于是「减 -5」变成「加 5」—— 补偿一次销量凭空涨 5，且所有分支都成功。
	Quantity int64 `json:"quantity"`
	// OrderNo 业务单号。**只作日志与排障锚点，不落 product_sales** ——
	// 那张表只有 (product_id, sold, updated_at) 三列，没有单号列（见
	// schema-micro.sql），凭空往 SQL 里加一列等于虚构 schema。
	OrderNo string `json:"order_no"`
}
