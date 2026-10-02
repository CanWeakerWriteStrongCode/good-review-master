package server

import (
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"good-review-master/config"
	"good-review-master/version"

	"github.com/gin-gonic/gin"
)

// 运行时诊断接口。设计取舍见下：
//
// 为什么 goroutine 分析**不**挂在 /api/debug/pprof 后面：
//   runtime.Stack(buf, true) 给出的就是 pprof `goroutine?debug=2` 的同一份数据，
//   自己取一次即可。这样子有三个好处：诊断页不依赖 runtime.enable_pprof
//   （那个开关有真实的采样开销、默认关闭，而诊断能力应该一直在线）；
//   解析在 Go 里做，可以单测；前端只负责渲染，符合本仓库"store 干活、页面只渲染"的既有分工。
//
// pprof 仍保留原职责：给标准库的文本视图与二进制 profile（供 go tool pprof 出火焰图）。

// GoroutineGroup 一类 goroutine 的聚合，按栈里第一个"非运行时"帧归类。
type GoroutineGroup struct {
	Frame string `json:"frame"` // 归类用的栈帧函数名
	State string `json:"state"` // 该类里出现最多的状态
	Count int    `json:"count"` // 该类 goroutine 数量
}

// GoroutinesInfo goroutine 概况。
type GoroutinesInfo struct {
	Total      int              `json:"total"`
	GroupCount int              `json:"group_count"`
	Groups     []GoroutineGroup `json:"groups"`
	// StackDump 仅在 ?full=1 时返回：自动刷新时每次都传几 MB 的栈是不可接受的。
	StackDump string `json:"stack_dump,omitempty"`
	// Truncated 表示缓冲区上限被打到、栈被截断（病态情况下才会出现）
	Truncated bool `json:"truncated,omitempty"`
}

// RuntimeInfo 进程运行概况。
type RuntimeInfo struct {
	Goroutines      int     `json:"goroutines"`
	HeapAllocBytes  uint64  `json:"heap_alloc_bytes"`
	HeapInuseBytes  uint64  `json:"heap_inuse_bytes"`
	HeapSysBytes    uint64  `json:"heap_sys_bytes"`
	StackInuseBytes uint64  `json:"stack_inuse_bytes"`
	NumGC           uint32  `json:"num_gc"`
	GCPauseTotalMs  float64 `json:"gc_pause_total_ms"`
	GOMAXPROCS      int     `json:"gomaxprocs"`
	NumCPU          int     `json:"num_cpu"`
	GoVersion       string  `json:"go_version"`
	UptimeSeconds   int64   `json:"uptime_seconds"`
	Version         string  `json:"version"`
	// PprofEnabled 让前端决定要不要显示 pprof 相关区块，避免前端再维护一份开关状态
	PprofEnabled bool `json:"pprof_enabled"`
}

// DiagnosticsData 是 GET /api/diagnostics 的响应体。
type DiagnosticsData struct {
	Runtime    RuntimeInfo    `json:"runtime"`
	Goroutines GoroutinesInfo `json:"goroutines"`
}

// goroutineDump 抓取全部 goroutine 的栈。第二个返回值表示是否因缓冲区上限被截断。
//
// runtime.Stack(all=true) 会 stop-the-world，代价随 goroutine 数增长——
// 这也是页面默认不开自动刷新的原因。
func goroutineDump() (string, bool) {
	const (
		// 初值刻意小：这个函数每次 /api/diagnostics 都会跑，开 1 MiB 的话
		// 单它一项就能占掉堆 profile 的前几名——一个诊断接口把自己的堆 profile
		// 污染成为自己，看泄漏的人会被工具本身误导（实测 1 MiB 时它占 31%）。
		// 实测 72 个 goroutine 的栈约 24 KB，64 KiB 足够覆盖常规情况，不够再翻倍。
		initialSize = 64 << 10
		maxSize     = 64 << 20 // 上限 64 MiB：防病态情况下无限翻倍打爆内存
	)
	buf := make([]byte, initialSize)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n]), false
		}
		if len(buf) >= maxSize {
			return string(buf[:n]), true
		}
		buf = make([]byte, min(len(buf)*2, maxSize))
	}
}

// runtimeSkipPrefixes 归类时要跳过的栈帧前缀。
//
// 必须跳过：parker 类 goroutine 的栈顶全是 runtime.gopark / sync.runtime_Semacquire，
// 不看穿这些运行时帧的话，成百上千个 goroutine 会全归成一类，分组就失去意义了。
//
// 前缀分带点与带斜杠两类，别只顾一种：
//   - "runtime."   → runtime.gopark 这类运行时内部函数
//   - "runtime/"   → runtime/pprof、runtime/debug 这些子包（取栈本身的开销帧，
//     永远不是你要找的元凶；写成 "runtime." 匹配不到带斜杠的，实测漏过一次）
//   - "internal/"  → internal/poll 等，netpoll 等待帧
var runtimeSkipPrefixes = []string{"runtime.", "runtime/", "sync.", "internal/", "os/signal."}

// groupGoroutines 把 runtime.Stack 的文本解析成按栈顶函数聚合的分组，按数量降序。
//
// 输入格式（debug=2 / runtime.Stack(all=true)）：
//
//	goroutine 18 [chan receive]:
//	main.worker(...)
//		/tmp/main.go:10 +0x45
//	created by main.main in goroutine 1
//		/tmp/main.go:6 +0x9e
//	<空行>
//
// 解析刻意做得宽容：认不出来的一律跳过，绝不因为格式细节变化而把整页搞崩——
// 这是给人看的诊断工具，宁可少显示一行也不能整个挂掉。
func groupGoroutines(dump string) GoroutinesInfo {
	blocks := strings.Split(strings.ReplaceAll(dump, "\r\n", "\n"), "\n\n")

	info := GoroutinesInfo{Groups: []GoroutineGroup{}}
	type agg struct {
		count  int
		states map[string]int
	}
	byFrame := make(map[string]*agg)

	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}

		// 块首行必须是 "goroutine <id> [<state>]:"
		header := strings.TrimSpace(lines[0])
		if !strings.HasPrefix(header, "goroutine ") {
			continue
		}
		state := parseGoroutineState(header)
		info.Total++

		frame := topBusinessFrame(lines[1:])
		if frame == "" {
			frame = "(无法识别)"
		}
		entry, ok := byFrame[frame]
		if !ok {
			entry = &agg{states: make(map[string]int)}
			byFrame[frame] = entry
		}
		entry.count++
		entry.states[state]++
	}

	for frame, entry := range byFrame {
		info.Groups = append(info.Groups, GoroutineGroup{
			Frame: frame,
			State: mostCommonState(entry.states),
			Count: entry.count,
		})
	}
	// 数量降序；同数量按名字排序，保证输出稳定（否则 map 遍历顺序会让刷新时表格乱跳）
	sort.Slice(info.Groups, func(i, j int) bool {
		if info.Groups[i].Count != info.Groups[j].Count {
			return info.Groups[i].Count > info.Groups[j].Count
		}
		return info.Groups[i].Frame < info.Groups[j].Frame
	})
	info.GroupCount = len(info.Groups)
	return info
}

// parseGoroutineState 从 "goroutine 18 [chan receive]:" 里取出 "chan receive"。
func parseGoroutineState(header string) string {
	open := strings.Index(header, "[")
	closeIdx := strings.LastIndex(header, "]")
	if open < 0 || closeIdx <= open {
		return ""
	}
	return header[open+1 : closeIdx]
}

// topBusinessFrame 返回栈里第一个"非运行时"帧的函数名；全是运行时帧时退回第一个帧。
func topBusinessFrame(lines []string) string {
	var firstFrame string
	for _, frame := range stackFrames(lines) {
		if firstFrame == "" {
			firstFrame = frame
		}
		skip := false
		for _, prefix := range runtimeSkipPrefixes {
			if strings.HasPrefix(frame, prefix) {
				skip = true
				break
			}
		}
		if !skip {
			return frame
		}
	}
	return firstFrame
}

// stackFrames 抽出一个栈块里的函数名，按栈顶到栈底顺序。
//
// 判定方式：函数名那行的**下一行**是 tab 缩进的 `file:line +0xoffset`。
// 用"看下一行"而不是"看本行缩进"，是因为 runtime 在不同版本里对函数行的缩进处理
// 并不稳定（有时顶格、有时带 tab），而文件行必定带 tab 这一点是稳的。
// 同时排除 "created by X in goroutine N"——它也满足"下一行是 tab 缩进"，但它不是栈帧。
func stackFrames(lines []string) []string {
	var frames []string
	for i := 0; i+1 < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		next := strings.TrimRight(lines[i+1], "\r")
		if !strings.HasPrefix(next, "\t") {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "created by ") {
			continue
		}
		frames = append(frames, normalizeFrame(line))
	}
	return frames
}

// normalizeFrame 把栈帧整成既可读、又能正确聚合的函数名。
func normalizeFrame(frame string) string {
	return unescapePackageDots(stripFrameArgs(frame))
}

// unescapePackageDots 还原包路径里被编译器转义的 '.'。
//
// 背景（cmd/internal/objabi/path.go 的 PathToPrefix）：符号表里包路径**最后一段**的 '.'
// 会被转义成 %2e，为的是区分 "包名.v2" 和 "包名.方法" 这种歧义。
// 于是 gopkg.in/natefinch/lumberjack.v2 在栈里长成 .../lumberjack%2ev2，
// 直接显示很难看。前面的 gopkg.in 不带 %2e，正是因为转义只作用于最后一段。
//
// 只替换 %2e 是安全的：PathToPrefix 会把真正的 '%' 转义成 %25，
// 所以符号里出现的 %2e 必然是那个被转义的 '.'，不存在把原文改错的可能。
func unescapePackageDots(frame string) string {
	return strings.ReplaceAll(frame, "%2e", ".")
}

// stripFrameArgs 去掉栈帧的参数列表，只留函数名。
//
// 这一步不是美化，是正确性所必需：参数里带指针地址，同一个函数在不同 goroutine
// 上参数不同，于是每个 goroutine 各成一组，聚合完全失效。例如
// `net/http.(*Server).Serve(0xc0003a4000, {0x7ff6a2c180d0, 0xc0003b2000})`
// 与另一个同函数的栈帧参数不同，不去掉就永远没人跟它归到一类。
//
// 从末尾反向扫配对括号，而不是取第一个 '('：函数名本身可能含括号
// （`(*Pool).worker` 的接收者），而参数列表实践中不含括号，反向配对最稳。
func stripFrameArgs(frame string) string {
	if !strings.HasSuffix(frame, ")") {
		return frame
	}
	depth := 0
	for i := len(frame) - 1; i >= 0; i-- {
		switch frame[i] {
		case ')':
			depth++
		case '(':
			depth--
			if depth == 0 {
				return frame[:i]
			}
		}
	}
	return frame // 括号不配对，原样返回
}

// mostCommonState 取出现次数最多的状态；同频次时按字典序取小的，保证结果稳定。
func mostCommonState(states map[string]int) string {
	best, bestCount := "", -1
	for state, count := range states {
		if count > bestCount || (count == bestCount && state < best) {
			best, bestCount = state, count
		}
	}
	return best
}

// handleDiagnostics 返回运行时概况 + goroutine 分组，?full=1 时附上栈全文。
func handleDiagnostics(cfg *config.Config, startedAt time.Time) gin.HandlerFunc {
	return func(c *gin.Context) {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)

		dump, truncated := goroutineDump()
		goroutines := groupGoroutines(dump)
		// 只有明确要全文时才带上，避免自动刷新反复传几 MB
		if c.Query("full") == "1" {
			goroutines.StackDump = dump
		}
		goroutines.Truncated = truncated

		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: DiagnosticsData{
				Runtime: RuntimeInfo{
					Goroutines:      runtime.NumGoroutine(),
					HeapAllocBytes:  mem.HeapAlloc,
					HeapInuseBytes:  mem.HeapInuse,
					HeapSysBytes:    mem.HeapSys,
					StackInuseBytes: mem.StackInuse,
					NumGC:           mem.NumGC,
					GCPauseTotalMs:  float64(mem.PauseTotalNs) / 1e6,
					GOMAXPROCS:      runtime.GOMAXPROCS(0),
					NumCPU:          runtime.NumCPU(),
					GoVersion:       runtime.Version(),
					UptimeSeconds:   int64(time.Since(startedAt).Seconds()),
					Version:         version.String(),
					PprofEnabled:    cfg.EnablePprof,
				},
				Goroutines: goroutines,
			},
		})
	}
}
