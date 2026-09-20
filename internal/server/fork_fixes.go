// ===========================================================================
//  fork_fixes.go —— fork 自有改动的「净化层」
//
//  为什么单独一个文件：
//  这个 fork 对上游做了三处行为增强（根路径直达面板、CORS 预检、配置写入的
//  bind mount 兜底）。早先它们直接改在 internal/server/handler.go 里，而该文件
//  是上游最活跃的文件之一（近 50 个提交里改了 16 次），导致每次 sync-upstream
//  都必然产生冲突，人工解冲突时又要重新理解两边语义。
//
//  现在把这些逻辑集中在这个文件，handler.go 里只留**一行调用接缝**：
//      if forkBeforeRouting(w, r, h.cfg.PanelRoot, h.cfg.Panel != nil) { return }
//  上游再怎么改 handler.go，只要不动 ServeHTTP 的那一行，合并就是全自动的。
//
//  本文件不依赖 handler 内部字段以外的任何东西，上游改动不会波及这里。
// ===========================================================================

package server

import (
	"net/http"
	"strings"
)

// rootProbePaths 挂根路径时**必须让出**的路径：这些是网关自身 API，不能被面板接管。
// 其余未匹配路径（含 /、/app.js、/api/*）在 PanelRoot 开启时重写到 /panel 前缀。
//
// ⚠️ 这里必须显式覆盖**全部**网关路由，不能依赖 ServeMux 的匹配优先级 ——
// ServeMux 只在"请求没被重写"时才按 pattern 具体度选择；一旦入口重写把 /v1/models
// 变成 /panel/v1/models，网关路由就再也匹配不到了。
// 2026-09-18 漏列 /v1/ 导致 /v1/models 与 /v1/chat/completions 全 404（面板吃掉了网关 API），
// 已由 TestPanelRootDoesNotShadowGatewayAPI 锁死为回归测试。
var rootProbePaths = map[string]bool{
	"/healthz": true,
	"/status":  true,
}

// isGatewayPath 判断请求路径是否属于网关自身 API（面板不得接管）。
// 覆盖 /status、/healthz 与 /v1 全族（/v1/models、/v1/chat/completions…）。
func isGatewayPath(p string) bool {
	if rootProbePaths[p] {
		return true
	}
	// /v1 及其子路径；注意 /v1x 不应被误认为网关路径。
	return p == "/v1" || strings.HasPrefix(p, "/v1/")
}

// rewriteToPanelRoot 把根路径形态的请求重写为 /panel 前缀形态，**原地改 r**。
//
// 设计意图：面板 handler 内部 36 条路由与前端 app.js 的 fetch 都硬编码了 /panel 前缀，
// 与其全量改写（易漏、且前端缓存旧版会立刻全 404），不如在入口处做一次重写 ——
// 挂载点变成可选项，而面板内部实现零改动。
//
// /panel/* 原样放行（保留旧链接与文档里的地址可用）。
func rewriteToPanelRoot(r *http.Request) {
	p := r.URL.Path
	if p == "/panel" || strings.HasPrefix(p, "/panel/") {
		return // 已经是 /panel 形态，不动
	}
	if isGatewayPath(p) {
		return // 让给网关 API（/healthz、/status、/v1/*）
	}
	if p == "/" {
		r.URL.Path = "/panel/"
		return
	}
	// /app.js → /panel/app.js；/api/overview → /panel/api/overview
	r.URL.Path = "/panel" + p
}

// corsPreflight 处理浏览器跨域（CORS）。
// 背景：网关与面板均为纯 Bearer 鉴权（无 Cookie 会话），放开 CORS 不存在
// 凭据自动携带类攻击面；而面板内置的密钥测试器、以及部署在其它域名下的
// 网页版聊天前端，都从浏览器发 fetch —— 没有 CORS 头时 OPTIONS 预检会被
// ServeMux 以 405 拒掉，浏览器直接报 "Failed to fetch"。
// 用 * 而非回显 Origin：无需 credentials，语义最简单且足够。
func corsPreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") == "" {
		return false
	}
	// 实际响应也要带 Allow-Origin（浏览器对正式请求同样校验）
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Max-Age", "86400")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// forkBeforeRouting 是 fork 侧统一的入口前置钩子，把两件事收在一处调用：
//  1. CORS 预检：OPTIONS + Origin 命中即直接 204 返回（返回 true 表示已处理完毕）
//  2. 根路径直达面板：panelRoot 且面板已挂载时，把 / 与 /api/* 重写为 /panel 前缀
//
// 返回 true 表示请求已由本函数终止，调用方应直接 return（不要再走 mux）。
//
// ⚠️ 上游对接说明：本函数是 handler.go 与 fork 逻辑之间的**唯一接缝**。
// ServeHTTP 里保持这一行不变，上游对 handler.go 的任何改动都不会与 fork 冲突
// （除 Config 结构体里的 PanelRoot 字段声明外 —— 那是 Go 语法上无法外置的）。
func forkBeforeRouting(w http.ResponseWriter, r *http.Request, panelRoot, panelMounted bool) bool {
	if corsPreflight(w, r) {
		return true
	}
	if panelRoot && panelMounted {
		rewriteToPanelRoot(r)
	}
	return false
}
