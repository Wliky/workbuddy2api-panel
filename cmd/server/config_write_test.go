package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// withFailingRename 临时把 renameFile 换成"永远返回 EBUSY"，
// 用于在单测里模拟 Docker **单文件 bind mount** 场景
// （真是挂载点导致的 EBUSY 无法在普通单测里制造，需要 mount 权限）。
func withFailingRename(t *testing.T) {
	t.Helper()
	orig := renameFile
	renameFile = func(_, _ string) error {
		// 与内核在挂载点上返回的错误一致：EBUSY → "device or resource busy"
		return &os.LinkError{Op: "rename", Old: "x.tmp", New: "x", Err: syscall.EBUSY}
	}
	t.Cleanup(func() { renameFile = orig })
}

// TestWriteFileAtomicFallsBackWhenRenameBusy 锁死降级行为：
// rename 返回 EBUSY 时，writeFileAtomic **必须**改成原地写并返回 nil，
// 而不是把 "device or resource busy" 抛给面板。
//
// 这是 2026-09-18 线上故障的看门测试。**已用变异测试验证**：
// 把降级分支删掉（退回裸 os.Rename）后本测试会 FAIL。
func TestWriteFileAtomicFallsBackWhenRenameBusy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")
	if err := os.WriteFile(target, []byte("old-content"), 0o600); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	withFailingRename(t)

	err := writeFileAtomic(target, []byte(`{"panel_root":true}`), 0o600)
	if err != nil {
		t.Fatalf("rename 返回 EBUSY 时应降级成功，实际报错: %v", err)
	}

	// 内容必须是新内容（证明原地写真的执行了）
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if string(got) != `{"panel_root":true}` {
		t.Errorf("降级后内容不符: %q", got)
	}

	// 不得残留 .tmp
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("降级后不应残留 .tmp 文件")
	}
}

// TestWriteFileAtomicStillErrorsOnWriteFailure 确认降级**只**针对 rename 失败：
// 写 tmp 本身失败必须照常报错，不能被降级逻辑吞掉。
//
// 为什么不用"只读目录"制造失败：Windows 的 os.Mkdir(dir, 0o555) **不产生只读目录**
// （权限位在 Windows 上是 no-op），测试会在 Windows 上假绿；而 root 跑 CI 时 POSIX
// 也一样无视权限位。这里改用平台无关的确定性失败：目标父目录不存在 → O_CREATE 必败。
func TestWriteFileAtomicStillErrorsOnWriteFailure(t *testing.T) {
	dir := t.TempDir()
	// 父目录不存在：os.Create 拿不到 ENOENT 之外的结果，任何平台都必失败。
	target := filepath.Join(dir, "no-such-dir", "config.json")

	err := writeFileAtomic(target, []byte("x"), 0o600)
	if err == nil {
		t.Fatal("父目录不存在时写入应报错")
	}
	// 错误必须提到写入环节，而不是被包装成 rename 降级的信息。
	if !strings.Contains(err.Error(), "失败") {
		t.Errorf("错误信息应指向写入失败，实际: %v", err)
	}
}

// TestWriteFileAtomicNoRenameIfTempWriteFails 确认写 tmp 失败时**不会**调用 rename
// （也就不会误入降级分支），从调用序列上锁死两个分支互不串台。
func TestWriteFileAtomicNoRenameIfTempWriteFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "no-such-dir", "config.json")

	renameCalled := 0
	orig := renameFile
	renameFile = func(o, n string) error { renameCalled++; return orig(o, n) }
	t.Cleanup(func() { renameFile = orig })

	if err := writeFileAtomic(target, []byte("x"), 0o600); err == nil {
		t.Fatal("应报错")
	}
	if renameCalled != 0 {
		t.Errorf("tmp 未写成时不应调用 rename，实际 %d 次", renameCalled)
	}
}

// TestWriteFileAtomicNormalPathUsesRename 确认正常路径仍走 rename（保持原子性），
// 且降级不会误触发。
func TestWriteFileAtomicNormalPathUsesRename(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")

	called := 0
	orig := renameFile
	renameFile = func(o, n string) error { called++; return orig(o, n) }
	t.Cleanup(func() { renameFile = orig })

	if err := writeFileAtomic(target, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("首次写入应成功: %v", err)
	}
	if called != 1 {
		t.Errorf("正常路径应走 rename，实际调用 %d 次", called)
	}

	// 覆盖已有文件同样走 rename
	if err := writeFileAtomic(target, []byte(`{"a":2}`), 0o600); err != nil {
		t.Fatalf("覆盖写入应成功: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != `{"a":2}` {
		t.Errorf("内容不符: %q", got)
	}
	if _, err := os.Stat(target + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("正常路径不应残留 .tmp")
	}
}

// TestMergeAndRenderPreservesUnknownKeysAndOverlays mergeAndRender 是保存配置的
// 前半程：保留磁盘上的未知键，同时叠加面板提交的键，并校验通过。
func TestMergeAndRenderPreservesUnknownKeysAndOverlays(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	base := map[string]any{
		"listen":       ":7863",
		"api_key":      "k",
		"auth_dir":     "./auths",
		"my_hand_note": "别删我", // 用户手写未知键
		"server":       map[string]any{"panel_root": false},
	}
	raw, _ := json.Marshal(base)
	if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
		t.Fatalf("准备配置失败: %v", err)
	}

	out, err := mergeAndRender(cfgPath, []byte(`{"server":{"panel_root":true}}`))
	if err != nil {
		t.Fatalf("合并渲染失败: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("产物不是合法 JSON: %v", err)
	}
	if got["my_hand_note"] != "别删我" {
		t.Errorf("未知键被洗掉了: %v", got["my_hand_note"])
	}
	srv, _ := got["server"].(map[string]any)
	if srv["panel_root"] != true {
		t.Errorf("提交的键未生效: %v", srv)
	}
	if got["listen"] != ":7863" {
		t.Errorf("兄弟顶层键丢失: %v", got["listen"])
	}
}

// TestMergeAndRenderRejectsInvalid 非法配置不得落盘（校验前置）。
//
// 为什么用 soft_rate 而不是 listen：normalize() 对 listen 是**宽松**的
// （不含冒号就补一个，任何字符串都会被接受），拿它当"非法输入"是错的假设。
// soft_rate 走 time.ParseDuration，拼错必错，是真正会被拒绝的字段。
func TestMergeAndRenderRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"listen":":7863"}`), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}

	// 1) 无法解析的时长
	if _, err := mergeAndRender(cfgPath, []byte(`{"cooldown":{"soft_rate":"随便写的"}}`)); err == nil {
		t.Error("非法 soft_rate 应被拒绝")
	}
	// 2) pool.cost_explore_interval 非法时长（上游 73fe1f8 起 server.max_body_mb 已退役，
	//    改用同为"必须能解析成时长"的 key 占位）。
	if _, err := mergeAndRender(cfgPath, []byte(`{"pool":{"cost_explore_interval":"乱写"}}`)); err == nil {
		t.Error("非法 cost_explore_interval 应被拒绝")
	}
	// 3) prompt.mode 枚举非法
	if _, err := mergeAndRender(cfgPath, []byte(`{"prompt":{"mode":"乱写"}}`)); err == nil {
		t.Error("非法 prompt.mode 应被拒绝")
	}
	// 4) 排程小时越界
	if _, err := mergeAndRender(cfgPath, []byte(`{"schedule":{"checkin_hours":[25]}}`)); err == nil {
		t.Error("越界小时应被拒绝")
	}

	// 校验失败时**不得**留下任何痕迹：文件内容必须还是原样。
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if string(got) != `{"listen":":7863"}` {
		t.Errorf("校验失败却被写盘了: %q", got)
	}
}

// TestMergeAndRenderRejectsMalformedJSON 提交上来的 JSON 坏掉时必须报错，
// 不能静默用空配置覆盖掉用户磁盘上的配置。
//
// 注意语义差别（刻意为之，别搞反）：
//   - **提交的** JSON 非法 → 报错（本测试）。
//   - **磁盘上已有的** JSON 非法 → 不报错，按空 map 处理（mergeAndRender 里
//     `cur = map[string]any{}`），从而让面板能"修好一个写坏的配置"。
func TestMergeAndRenderRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"listen":":7863"}`), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	if _, err := mergeAndRender(cfgPath, []byte(`{"listen":`)); err == nil {
		t.Error("提交非法 JSON 应被拒绝")
	}
	// 磁盘原文件必须没被动过
	got, _ := os.ReadFile(cfgPath)
	if string(got) != `{"listen":":7863"}` {
		t.Errorf("提交非法 JSON 却改动了磁盘文件: %q", got)
	}
}

// TestMergeAndRenderRecoversFromCorruptOnDisk 磁盘配置写坏时，面板仍能保存一份好配置
// （这是 mergeAndRender 对已有文件解析失败时按空 map 处理的真正价值）。
func TestMergeAndRenderRecoversFromCorruptOnDisk(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// 典型现场：有人手工追加了字符导致顶层值后多出一个 token。
	if err := os.WriteFile(cfgPath, []byte(`{"listen":":7863"}x`), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	out, err := mergeAndRender(cfgPath, []byte(`{"listen":":7863"}`))
	if err != nil {
		t.Fatalf("磁盘配置损坏时应能恢复，实际报错: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("产物不是合法 JSON: %v", err)
	}
	if got["listen"] != ":7863" {
		t.Errorf("恢复后的配置不符: %v", got)
	}
}
