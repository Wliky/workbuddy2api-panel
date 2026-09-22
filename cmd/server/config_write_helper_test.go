package main

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// saveTestRig 是 saveConfig 需要的最小依赖装配。
//
// 为什么可以让这些依赖"非真实"：本文件测的是 saveConfig 的**落盘段**行为
// （第 3 段）与**校验前置**语义（第 2 段），热应用段（第 4 段）只是把已校验的
// 值写进各组件，不是被测对象。用真构造函数即可，它们都是纯内存的：
//   - pool.New("")：stateFp 为空 → 不 load、不起 flusher goroutine，零副作用
//   - upstream.New()：只填默认字段与 http.Client，不发请求
//   - livecfg.New()：包一个空快照
//   - scheduler.New()：纯内存构造；Reconfigure 仅改字段 + 往 cap=1 的 channel
//     发非阻塞信号
//
// ⚠️ 若将来 saveConfig 的签名或热应用段需要新的依赖，改这里一处即可。
// ⚠️ 不要给这些组件接真实后端（网络/磁盘）——会让本测试从单元测试退化成集成测试。
type saveTestRig struct {
	live *livecfg.Holder
	pool *pool.Pool
	up   *upstream.Client
	sch  *scheduler.Scheduler
}

func newSaveTestRig(t *testing.T) *saveTestRig {
	t.Helper()
	p := pool.New("") // 空 stateFp：无 I/O、无后台 goroutine
	t.Cleanup(p.Close)
	return &saveTestRig{
		live: livecfg.New(livecfg.Snapshot{}),
		pool: p,
		up:   upstream.New(),
		sch:  scheduler.New(scheduler.Config{}),
	}
}
