package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ===========================================================================
//  config_write_test.go —— 保存配置落盘行为的看门测试
//
//  ⚠️ 本文件 2026-09-23 做过一次重写，背景必须知道：
//
//  降级逻辑原先在 fork 的 cmd/server/fork_save.go，靠 `renameFile` 变量注入
//  来模拟 EBUSY（因为真实的挂载点 EBUSY 需要 mount 权限，单测里造不出来）。
//  上游 5e1422c + ab9a162 自己实现了同一语义后，fork 侧实现已删除、改用上游。
//
//  上游把逻辑**内联在 saveConfig 里**、直接调 os.Rename，没有可注入的间接层。
//  因此这里不能再靠替换函数指针来造 EBUSY，改为**测 saveConfig 的可观测行为**：
//  正常路径（能 rename）必须原子替换且不留 tmp，这是回归风险最高的一条。
//
//  降级分支（EBUSY → 原地写）在单测里造不出来，改由设备侧冒烟覆盖 ——
//  wb2api-pull-update.sh 的「保存配置」一项就是它的验收。
// ===========================================================================

// writeCfg 写一份最小可解析的配置文件，返回路径。
func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("准备配置文件失败: %v", err)
	}
	return p
}

// TestSaveConfigWritesAndKeepsUnknownKeys 锁死 saveConfig 落盘的基本契约：
//   - 面板提交的键生效
//   - 磁盘上手写的未知键不被洗掉
//   - 正常路径走 tmp+rename，不留 .tmp 残留
//   - 落盘产物是合法 JSON
//
// 这是「面板保存配置」这条链路的核心回归保护：saveConfig 每次被上游改动
// （热应用字段列表、校验顺序等）都可能顺手碰坏落盘段，本测试是那一段的哨兵。
func TestSaveConfigWritesAndKeepsUnknownKeys(t *testing.T) {
	base := `{
	  "listen": ":7863",
	  "api_key": "k",
	  "auth_dir": "./auths",
	  "my_hand_note": "别删我",
	  "server": {"panel_root": false}
	}`
	path := writeCfg(t, base)

	// live/pool/upstream/scheduler 传 nil：本测试只关心第 3 段落盘，
	// 且 saveConfig 会在校验失败或 marshal 失败时提前返回，
	// 走到热应用前就已是我们要验证的状态。nil 会在热应用处 panic，
	// 所以这里用一个最小装配（见 helper）。
	h := newSaveTestRig(t)

	if _, err := saveConfig([]byte(`{"server":{"panel_root":true}}`), path, h.live, h.pool, h.up, h.sch); err != nil {
		t.Fatalf("保存配置失败: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回落盘结果失败: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("落盘产物不是合法 JSON: %v\n%s", err, got)
	}
	if m["my_hand_note"] != "别删我" {
		t.Errorf("未知键被洗掉了: %v", m["my_hand_note"])
	}
	srv, _ := m["server"].(map[string]any)
	if srv == nil || srv["panel_root"] != true {
		t.Errorf("提交的键未生效: %v", m["server"])
	}
	if m["listen"] != ":7863" {
		t.Errorf("兄弟顶层键丢失: %v", m["listen"])
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("正常路径不应残留 .tmp 文件")
	}
}

// TestSaveConfigRejectsInvalidAndLeavesFileIntact 校验失败绝不落盘。
//
// 这条比"报错"更重要：**文件必须逐字节保持原样**。
// 上游把校验放在落盘之前（// 2) 校验），本测试锁死这个顺序不被调换。
func TestSaveConfigRejectsInvalidAndLeavesFileIntact(t *testing.T) {
	const orig = `{"listen":":7863"}`
	path := writeCfg(t, orig)
	h := newSaveTestRig(t)

	bad := []struct {
		name string
		body string
	}{
		{"非法时长 soft_rate", `{"cooldown":{"soft_rate":"随便写的"}}`},
		{"非法时长 cost_explore_interval", `{"pool":{"cost_explore_interval":"乱写"}}`},
		{"枚举越界 prompt.mode", `{"prompt":{"mode":"乱写"}}`},
		{"小时越界 checkin_hours", `{"schedule":{"checkin_hours":[25]}}`},
		{"提交的 JSON 本身坏掉", `{"listen":`},
	}
	for _, c := range bad {
		if _, err := saveConfig([]byte(c.body), path, h.live, h.pool, h.up, h.sch); err == nil {
			t.Errorf("%s: 应被拒绝，实际通过", c.name)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: 读回失败: %v", c.name, err)
		}
		if string(got) != orig {
			t.Errorf("%s: 校验失败却改动了磁盘文件: %q", c.name, got)
		}
	}
}

// TestSaveConfigRecoversFromCorruptOnDisk 磁盘配置写坏时，面板仍能存回一份好配置。
//
// 语义上刻意与上一条相反，别搞反：
//   - **提交上来的** JSON 非法 → 报错（上一条）
//   - **磁盘上已有的** JSON 非法 → 按空 map 处理，让面板能把坏配置修好（本一条）
func TestSaveConfigRecoversFromCorruptOnDisk(t *testing.T) {
	path := writeCfg(t, `{"listen":":7863"}x`) // 顶层值后多一个 token
	h := newSaveTestRig(t)

	if _, err := saveConfig([]byte(`{"listen":":7863"}`), path, h.live, h.pool, h.up, h.sch); err != nil {
		t.Fatalf("磁盘配置损坏时应能恢复，实际报错: %v", err)
	}
	got, _ := os.ReadFile(path)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("恢复后的产物不是合法 JSON: %v", err)
	}
	if m["listen"] != ":7863" {
		t.Errorf("恢复后的配置不符: %v", m)
	}
}

// TestSaveConfigWriteFailureSurfacesError 落盘真的失败时必须把错误抛出来，
// 不能被静默吞掉导致面板显示"保存成功"但磁盘没变。
//
// 制造方式：先让配置文件**存在且可读**（否则 saveConfig 会在第 1 步
// os.ReadFile 就返回 "read current config"，测不到落盘段），
// 再把 tmp 路径 **占成一个目录** —— os.WriteFile 打开 <path>.tmp 时会拿到
// "is a directory"，属于平台无关的确定性失败，且在 Windows 上也成立。
//
// 为什么不用"只读目录"：Windows 的 Mkdir(dir, 0o555) 不产生只读目录
// （权限位在 Windows 上是 no-op），测试会假绿；root 跑 CI 时 POSIX 也一样无视权限位。
func TestSaveConfigWriteFailureSurfacesError(t *testing.T) {
	path := writeCfg(t, `{"listen":":7863"}`) // 文件存在且可读 → 能走到落盘段

	// 把 config.json.tmp 占成目录，使 os.WriteFile(tmp) 必败。
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatalf("准备 tmp 占位目录失败: %v", err)
	}

	h := newSaveTestRig(t)
	_, err := saveConfig([]byte(`{"listen":":7863"}`), path, h.live, h.pool, h.up, h.sch)
	if err == nil {
		t.Fatal("写 tmp 失败时应报错，实际返回 nil（会被面板误报成功）")
	}
	if !strings.Contains(err.Error(), "write config") {
		t.Errorf("错误应指向写 tmp 环节，实际: %v", err)
	}

	// 原配置文件必须保持原样（落盘失败不得污染已有内容）。
	got, _ := os.ReadFile(path)
	if string(got) != `{"listen":":7863"}` {
		t.Errorf("落盘失败却改动了原配置: %q", got)
	}
}
