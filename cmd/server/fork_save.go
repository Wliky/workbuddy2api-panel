// ===========================================================================
//  fork_save.go —— fork 自有的配置落盘逻辑（净化层）
//
//  为什么单独一个文件：
//  上游 saveConfig 的落盘用的是「写 tmp + os.Rename」，在 Docker **单文件 bind
//  mount** 下会失败 —— 目标路径就是挂载点本身，内核拒绝 rename 替换挂载点并返回
//  EBUSY（"device or resource busy"）。这不是权限问题，chown/chmod 都无效。
//
//  这个 bug 的修复（writeFileAtomic 降级原地写）早先直接改在 main.go 里，
//  而 main.go 是上游最活跃的文件（近 50 个提交里改了 24 次），导致每次
//  sync-upstream 都要人工解冲突。
//
//  现在落盘逻辑集中在本文件，main.go 的 saveConfig 里只留**一行调用接缝**：
//      if err := writeFileAtomicResilient(path, out, 0o600); err != nil { return nil, err }
//  上游改 saveConfig 的其它部分（校验顺序、热应用字段列表）都不会与 fork 冲突。
//
//  ⚠️ 本文件同时是 mergeAndRender 的宿主。上游没有这个函数（它把「合并→校验→
//  渲染」内联在 saveConfig 里）；fork 抽出来是为了让"校验失败绝不落盘"成为
//  结构性保证，而不是靠调用顺序的约定。
// ===========================================================================

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
)

// writeFileInPlace 原地覆盖写入（truncate + write），**不做 rename**。
//
// 用于 rename 不可用的目标：Docker **单文件 bind mount**（如 /app/config.json）。
// 容器内对该路径 rename 会被内核拒绝并返回 EBUSY（"device or resource busy"），
// 因为 rename 不能替换一个挂载点 —— 这不是权限问题，chown/chmod 都救不了。
//
// 代价：本函数不提供原子性。写入中途掉电/被杀可能留下截断的文件。
// 因此仅在 rename 失败时降级使用，且降级前已把新内容完整算出（内容本身不会算错）。
func writeFileInPlace(path string, data []byte, perm os.FileMode) error {
	// O_TRUNC 复用现有 inode → 保住挂载关系（这正是我们要的：写穿到宿主文件）。
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// renameFile 是 os.Rename 的间接引用，**仅为测试可注入**：
// 单文件 bind mount 导致的 EBUSY 无法在单测里真实制造（需要挂载权限），
// 故测试用替换本变量的方式强制 rename 失败，从而覆盖降级分支。
var renameFile = os.Rename

// writeFileAtomic 先写 tmp 再 rename 做原子替换；**rename 失败时降级为原地写**。
//
// 降级分支是为 Docker 单文件 bind mount 场景准备的 —— 2026-09-18 线上故障：
// 容器把 config.json 以单文件 bind mount 注入，面板保存配置报
// "replace config: rename /app/config.json.tmp /app/config.json: device or resource busy"。
// 有了降级，这类部署也能正常保存配置。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		// 写 tmp 都失败（多为目录不可写）→ 直接返回，附上可操作提示。
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("写入 %s 失败: %w\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。"+
				"解法任选：1) 以本机 uid 运行容器；2) chown -R 10001:10001 <挂载目录>；"+
				"3) compose 设 user: \"0:0\"）", tmp, err)
		}
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := renameFile(tmp, path); err != nil {
		// 目标不可 rename（典型：单文件 bind mount 挂载点）→ 清理 tmp，降级原地写。
		_ = os.Remove(tmp)
		if werr := writeFileInPlace(path, data, perm); werr != nil {
			return fmt.Errorf("replace config: %w（降级原地写也失败: %v）", err, werr)
		}
		log.Printf("config: %s 不支持原子替换（多为单文件 bind mount），已降级为原地写入", path)
		return nil
	}
	return nil
}

// writeFileAtomicResilient 是 main.go 调用落盘的**唯一入口**（接缝函数）。
//
// 存在的意义是把「选哪个落盘实现」的决策也收进本文件 —— 上游若哪天自己加了
// bind mount 兜底，只需改这里一行，main.go 完全不用动。
func writeFileAtomicResilient(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm)
}

// mergeAndRender 读现有配置 → 叠加提交的键（保留未知键）→ 校验 → 渲染 JSON 字节。
// 不落盘；落盘由调用方用 writeFileAtomic 完成（便于测试与失败前置）。
func mergeAndRender(path string, incomingRaw []byte) ([]byte, error) {
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(incomingRaw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	if _, err := ParseConfig(mergedJSON(merged)); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	return out, nil
}
