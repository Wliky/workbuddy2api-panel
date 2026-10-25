// ===========================================================================
//  fork_fixes.go —— fork 自有逻辑的「净化层」
//
//  本 fork 只增加一个行为：**根路径直达面板**（`server.panel_root = true` 时，
//  访问 `/` 直接就是面板首页，地址栏不出现 /panel）。
//
//  为什么单独一个文件：
//  internal/server/handler.go 是上游最活跃的文件之一。把逻辑收在这个文件里，
//  上游那边只留三处**最小接缝**，只要它们不动，sync-upstream 就是全自动合并：
//    internal/server/handler.go   Config.PanelRoot 字段声明 + ServeHTTP 首行调用
//    cmd/server/config.go         Server.PanelRoot 字段声明
//    cmd/server/main.go           装配时传一次 cfg.Server.PanelRoot
//
//  面板内部 36 条路由与前端 fetch 都硬编码了 /panel 前缀，与其全量改写（易漏、
//  且浏览器缓存旧版前端会立刻全 404），不如在入口处做一次路径重写 —— 挂载点变成
//  可选项，面板自身实现零改动，`/panel/*` 老地址也继续可用。
// ===========================================================================

package server

import (
	"net/http"
	"strings"
)

// gatewayPaths 面板**不得接管**的网关自身路由（精确匹配）。
//
// ⚠️ 必须显式列全，不能依赖 ServeMux 的匹配优先级：ServeMux 只在「请求没被重写」
// 时才按 pattern 具体度选择；一旦入口重写把 /v1/models 变成 /panel/v1/models，
// 网关路由就再也匹配不到了。上一个 fork 在 2026-09-18 漏列 /v1/ 曾导致
// /v1/models 与 /v1/chat/completions 全 404（面板吃掉了网关 API）。
//
// 本项目的网关路由比上一代多，逐个补上：
//
//	/messages          Anthropic Messages 别名（注意：没有 /v1 前缀）
//	/v2/chat/busy      官方客户端的「模型忙」预检
//	/metrics           Prometheus 指标（metrics.enabled 时才挂）
//	/admin/*           管理任务触发（admin.enabled 时才挂）
var gatewayPaths = map[string]bool{
	"/healthz":  true,
	"/status":   true,
	"/messages": true,
	"/metrics":  true,
}

// gatewayPrefixes 前缀族：/v1/*（chat/responses/messages/models/images/search/fetch 等）、
// /v2/*、/admin/*。
var gatewayPrefixes = []string{"/v1/", "/v2/", "/admin/"}

// isGatewayPath 判断是否属于网关自身 API（面板不得接管）。
func isGatewayPath(p string) bool {
	if gatewayPaths[p] {
		return true
	}
	for _, pre := range gatewayPrefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	// 裸前缀本身（/v1、/v2、/admin）
	return p == "/v1" || p == "/v2" || p == "/admin"
}

// rewriteToPanelRoot 把根路径形态的请求重写成 /panel 前缀形态，**原地改 r**。
//
//	/              → /panel/
//	/app.js        → /panel/app.js
//	/api/overview  → /panel/api/overview
//	/panel/*       → 原样放行
//	/v1/* 等网关路由 → 原样放行
func rewriteToPanelRoot(r *http.Request) {
	p := r.URL.Path
	if p == "/panel" || strings.HasPrefix(p, "/panel/") {
		return // 已经是 /panel 形态
	}
	if isGatewayPath(p) {
		return // 让给网关 API
	}
	if p == "/" {
		r.URL.Path = "/panel/"
		return
	}
	r.URL.Path = "/panel" + p
}

// forkBeforeRouting 是 handler.go 与 fork 逻辑之间的唯一入口。
// 返回 true 表示请求已处理完毕，调用方应直接 return（当前实现不会返回 true，
// 保留返回值是为了将来加 CORS 预检之类的短路逻辑时不改接缝签名）。
func forkBeforeRouting(w http.ResponseWriter, r *http.Request, panelRoot, panelMounted bool) bool {
	if panelRoot && panelMounted {
		rewriteToPanelRoot(r)
	}
	_ = w
	return false
}
