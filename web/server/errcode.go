package server

// 本文件是面板 API 唯一的错误码清单。
//
// # 为什么不用 HTTP 状态码表达业务结论
//
// HTTP 状态码说的是"这次请求在传输/服务层面成没成"，业务错误是"业务上允不允许"。
// 两者的读者不同：前者给网关、负载均衡、探针看；后者给前端弹窗看。
// 把业务结论塞进状态码，前端就得在每个页面各写一遍 if (status === 404)，
// 拦截器也没法统一处理——所以业务错误一律 HTTP 200，结论放 body 的 code 里。
//
// # 判据
//
//	请求走完业务代码、业务代码做出了判断 → HTTP 200 + body.code = 业务码
//	请求没走完业务代码（路由/方法/鉴权/限流），或服务器自己扛不住 → HTTP 非 200
//
// # 分段
//
//	200      成功
//	4xxxx    请求本身或身份有问题（解析失败、没 token、密码错、被限流）
//	5xxxx    服务器自己出问题（未预期错误、签 token 失败、读库失败）
//	6xxxx    业务规则拒绝：请求完全正常，业务上不允许（资源不存在、必须指定账号）
//
// 6xxxx 这一段的含义很关键：它出现时说明整条链路都是健康的，
// 只是这个操作按规则不能做。告警规则可以据此把"技术故障"和"业务拒绝"分成两条曲线。
//
// # 前端不认码
//
// 前端只判断 code != 200，然后弹 msg（后端已按 X-Lang 翻译好）。
// 码的作用只有三处：日志里能搜、Grafana 能分组、把 i18n 的 message id 钉住。
// 所以**加码时不用改前端**。
type ErrCode int

// 错误码清单。按段排列，加码时插到对应段里，不要按时间顺序往末尾追加——
// 乱序编号等于没有分类，线上看到一个码还得回代码里查。
const (
	// CodeOK 成功。业务信封里只有这一个"没问题"的取值。
	CodeOK ErrCode = 200

	// ---- 4xxxx：请求本身或身份有问题 ----

	// CodeBadJSON 请求体不是合法 JSON。业务代码第一行就没过去。
	CodeBadJSON ErrCode = 40000
	// CodeUnauthorized token 缺失或已过期，被鉴权中间件拦在业务代码之前。
	CodeUnauthorized ErrCode = 40100
	// CodeBadCredentials 账号或密码错误。这是业务判断（比对过了、不相等），
	// 所以配 HTTP 200，见 errTable。
	CodeBadCredentials ErrCode = 40101
	// CodeRateLimited 同一来源登录失败次数过多，被限流器挡住。
	CodeRateLimited ErrCode = 40103

	// ---- 5xxxx：服务器自己出问题 ----

	// CodeInternal 未预期的错误。前端只看到一句兜底文案 + 单号，详情进日志。
	CodeInternal ErrCode = 50000
	// CodeTokenSignFailed 签发登录 token 失败（密钥或签名环节出错）。
	CodeTokenSignFailed ErrCode = 50001
	// CodeAlertsReadFailed 读 alerts 表失败（库损坏、被锁、文件没了）。
	CodeAlertsReadFailed ErrCode = 50002

	// ---- 6xxxx：业务规则拒绝 ----

	// CodeWorkspaceNotFound 指定的工作空间不存在（账号被删掉或 ID 写错）。
	CodeWorkspaceNotFound ErrCode = 60001
	// CodeNoWorkspace 还没有任何工作空间。只在测试模式调调试接口时可能出现。
	CodeNoWorkspace ErrCode = 60002
	// CodeMultipleWorkspaces 存在多个工作空间，这个接口必须指定是哪一个。
	// 不指定就会把两个账号的数据混在一起，所以宁可拒绝也不猜。
	CodeMultipleWorkspaces ErrCode = 60003
)

// errEntry 是一个错误码的全部静态信息。
//
// **文案不在这里**：文案在 locales/*.yaml 里，按 ID 查。
// 一份码表里放两样文案（中文 + 英文）等于把语言写死在代码里，
// 那正要上 i18n 就白上了。
type errEntry struct {
	// ID 是 i18n 的 message id，也是日志和告警里认这条错误的名字。
	// 一旦发布**不要改**：前端缓存、告警规则、历史日志都靠它对上。
	ID string
	// HTTP 是这条错误该配的 HTTP 状态码。
	// 业务判断失败（密码错、资源不存在）给 200——HTTP 层没出错；
	// 请求没走完业务代码或服务器故障，给对应的 4xx/5xx。
	HTTP int
}

// errTable 是码表本体。加错误码时**只改这里 + locales 里的两份文案**，
// 中间件、前端都不需要动。
//
//nolint:gochecknoglobals // 全表只读，没有任何代码改它，包级常量表比构造注入更直白
var errTable = map[ErrCode]errEntry{
	CodeBadJSON:        {ID: "request.bad_json", HTTP: 400},
	CodeUnauthorized:   {ID: "auth.unauthorized", HTTP: 401},
	CodeBadCredentials: {ID: "auth.bad_credentials", HTTP: 200},
	CodeRateLimited:    {ID: "auth.rate_limited", HTTP: 429},

	CodeInternal:         {ID: "error.internal", HTTP: 500},
	CodeTokenSignFailed:  {ID: "auth.token_sign_failed", HTTP: 500},
	CodeAlertsReadFailed: {ID: "alerts.read_failed", HTTP: 500},

	CodeWorkspaceNotFound:  {ID: "workspace.not_found", HTTP: 200},
	CodeNoWorkspace:        {ID: "workspace.none", HTTP: 200},
	CodeMultipleWorkspaces: {ID: "workspace.multiple", HTTP: 200},
}

// entry 取错误码的静态信息。表里漏了码时返回一份"内部错误"的兜底，
// 而不是 panic：线上宁可少一条精确文案，也不能让一个漏登记的码把请求打成 500 空响应。
func (c ErrCode) entry() errEntry {
	if e, ok := errTable[c]; ok {
		return e
	}
	return errTable[CodeInternal]
}

// messageID 返回 i18n 的 message id，日志与指标里用这个而不是数字码。
// 数字码换个表就没人认识，名字（workspace.not_found）自带含义。
func (c ErrCode) messageID() string {
	return c.entry().ID
}

// isServerFault 表示这条错误归咎于服务器自己（5xxxx 段）。
//
// 用来决定要不要把 trace 的 span 标成 error：4xxxx 是调用方的问题、
// 6xxxx 是业务规则拒绝，这两类都是正常现象，标红只会把真正的故障淹掉。
func (c ErrCode) isServerFault() bool {
	return c >= CodeInternal && c < 60000
}

// allErrCodes 返回码表里的全部码，供测试核对"每个码都有文案"。
// 用切片而不是直接暴露 map：调用方只该遍历，不该改。
func allErrCodes() []ErrCode {
	codes := make([]ErrCode, 0, len(errTable))
	for code := range errTable {
		codes = append(codes, code)
	}
	return codes
}
