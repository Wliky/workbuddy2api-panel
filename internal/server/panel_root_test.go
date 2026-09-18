package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRewriteToPanelRoot 校验根路径 → /panel 前缀的重写映射。
// 这是 PanelRoot 特性的核心不变量，出错会导致面板全 404 或吃掉网关 API。
func TestRewriteToPanelRoot(t *testing.T) {
	cases := []struct {
		in   string
		want string
		why  string
	}{
		// —— 重写到面板 ——
		{"/", "/panel/", "根路径 → 面板首页"},
		{"/app.js", "/panel/app.js", "前端静态资源"},
		{"/api/overview", "/panel/api/overview", "面板 API"},
		{"/api/accounts/u1/tasks", "/panel/api/accounts/u1/tasks", "带路径参数"},
		{"/favicon.ico", "/panel/favicon.ico", "其它未匹配路径一律归面板"},

		// —— 让给网关 API（绝不能重写）——
		{"/healthz", "/healthz", "探活接口必须保留原语义"},
		{"/status", "/status", "网关状态接口"},
		{"/v1/models", "/v1/models", "OpenAI 兼容：模型列表"},
		{"/v1/chat/completions", "/v1/chat/completions", "OpenAI 兼容：对话（核心业务路径）"},
		{"/v1", "/v1", "裸 /v1"},
		{"/v1x", "/panel/v1x", "前缀相似但不是网关路径，应归面板"},

		// —— 已是 /panel 形态，原样放行（向后兼容）——
		{"/panel/", "/panel/", "旧链接可用"},
		{"/panel/app.js", "/panel/app.js", "旧静态资源路径"},
		{"/panel", "/panel", "/panel 交给 ServeMux 自己重定向到 /panel/"},
		{"/api", "/panel/api", "裸 /api 也归面板（面板无此路由 → 其内部 404）"},
	}

	for _, c := range cases {
		r := httptest.NewRequest("GET", c.in, nil)
		rewriteToPanelRoot(r)
		if r.URL.Path != c.want {
			t.Errorf("%s: rewrite(%q) = %q, want %q", c.why, c.in, r.URL.Path, c.want)
		}
	}
}

// TestPanelRootDoesNotShadowGatewayAPI 锁死"根路径挂载不得吃掉网关 API"这条约束。
// 用真实的 Handler + 一个受保护路由（/status）验证：开了 PanelRoot 后，
// 网关路由仍走自己的 handler，而不是被面板接管。
func TestPanelRootDoesNotShadowGatewayAPI(t *testing.T) {
	p := testPoolWith()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	panelHit := 0
	fakePanel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panelHit++
		w.WriteHeader(http.StatusTeapot) // 418：一旦被面板接管就能立刻看出来
	})

	h := NewHandler(Config{
		Pool:       p,
		Upstream:   up,
		APIKey:     "k",
		Panel:      fakePanel,
		PanelRoot:  true,
		RedisMode:  "noop",
		PromptMode: "custom",
	})

	// /healthz 是公开的，应返回 200（网关 handler，不是面板的 418）
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code == http.StatusTeapot {
		t.Fatal("/healthz 被面板接管了——网关探活接口被遮蔽")
	}
	if panelHit != 0 {
		t.Errorf("/healthz 不应进面板，panelHit=%d", panelHit)
	}

	// /status 需要鉴权：无 key → 401（仍由网关 withAuth 处理，不是面板 418）
	panelHit = 0
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code == http.StatusTeapot {
		t.Fatal("/status 被面板接管了")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/status 无 key 应 401，got %d", rec.Code)
	}
	if panelHit != 0 {
		t.Errorf("/status 不应进面板，panelHit=%d", panelHit)
	}

	// /v1/models 是真实业务路径：必须走网关（带 key → 200，不是面板 418，也不是 404）。
	// 2026-09-18 正是因为漏了这条，PanelRoot 上线后 /v1/models 与 /v1/chat/completions
	// 被重写成 /panel/v1/... 而全 404 —— 此断言即那次回归的看守。
	panelHit = 0
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusTeapot {
		t.Fatal("/v1/models 被面板接管了——OpenAI 兼容接口被遮蔽")
	}
	if rec.Code == http.StatusNotFound {
		t.Fatal("/v1/models 404 —— 被重写到了面板前缀，网关路由已不可达")
	}
	if panelHit != 0 {
		t.Errorf("/v1/models 不应进面板，panelHit=%d", panelHit)
	}

	// 根路径应进面板（418）
	panelHit = 0
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("根路径应进面板（418），got %d", rec.Code)
	}
	if panelHit != 1 {
		t.Errorf("根路径应命中面板 1 次，got %d", panelHit)
	}
}

// TestPanelRootDisabledByDefault PanelRoot=false 时根路径不应进面板（保持原行为）。
func TestPanelRootDisabledByDefault(t *testing.T) {
	p := testPoolWith()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	panelHit := 0
	fakePanel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panelHit++
		w.WriteHeader(http.StatusTeapot)
	})

	h := NewHandler(Config{
		Pool: p, Upstream: up, APIKey: "k", Panel: fakePanel,
		PromptMode: "custom", // PanelRoot 缺省 false
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if panelHit != 0 {
		t.Errorf("PanelRoot 未开启时根路径不应进面板，panelHit=%d", panelHit)
	}
}
