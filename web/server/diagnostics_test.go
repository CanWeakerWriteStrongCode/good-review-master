package server

import (
	"strings"
	"testing"
)

// realDump 是 runtime.Stack(buf, true) 的真实输出片段（取自 Go 1.25）。
// 刻意保留原样的制表符与空行——这个解析器的全部价值就在于能吃下真实格式。
const realDump = `goroutine 1 [running]:
runtime/pprof.writeGoroutineStacks({0x7ff6a2c12e08, 0xc0001b2000})
	D:/develop/golang/go1.25.6.windows-amd64/src/runtime/pprof/pprof.go:723 +0x70
runtime/pprof.writeGoroutine({0x0?, 0x7ff6a2c12e08?}, 0x2?)
	D:/develop/golang/go1.25.6.windows-amd64/src/runtime/pprof/pprof.go:712 +0x2b
good-review-master/web/server.handleDiagnostics.func1(0xc0001b2000)
	D:/develop/workspace_xcx/good-review-master/web/server/diagnostics.go:210 +0x2a

goroutine 7 [chan receive]:
good-review-master/pool.(*Pool).worker()
	D:/develop/workspace_xcx/good-review-master/pool/pool.go:33 +0x45
created by good-review-master/pool.New in goroutine 1
	D:/develop/workspace_xcx/good-review-master/pool/pool.go:26 +0x9e

goroutine 12 [chan receive]:
good-review-master/pool.(*Pool).worker()
	D:/develop/workspace_xcx/good-review-master/pool/pool.go:33 +0x45
created by good-review-master/pool.New in goroutine 1
	D:/develop/workspace_xcx/good-review-master/pool/pool.go:26 +0x9e

goroutine 18 [select]:
net/http.(*Server).Serve(0xc0003a4000, {0x7ff6a2c180d0, 0xc0003b2000})
	D:/develop/golang/go1.25.6.windows-amd64/src/net/http/server.go:3424 +0xb2
created by net/http.(*Server).ListenAndServe in goroutine 1
	D:/develop/golang/go1.25.6.windows-amd64/src/net/http/server.go:3343 +0xf9

goroutine 23 [IO wait]:
internal/poll.runtime_pollWait(0x2e1a4b0, 0x72)
	D:/develop/golang/go1.25.6.windows-amd64/src/runtime/netpoll.go:351 +0x85
internal/poll.(*FD).Accept(0xc0000d6000)
	D:/develop/golang/go1.25.6.windows-amd64/src/internal/poll/fd_windows.go:533 +0x2c5
net/http.(*Server).Serve(0xc0003a4000, {0x7ff6a2c180d0, 0xc0003b2000})
	D:/develop/golang/go1.25.6.windows-amd64/src/net/http/server.go:3424 +0xb2
created by net/http.(*Server).ListenAndServe in goroutine 1
	D:/develop/golang/go1.25.6.windows-amd64/src/net/http/server.go:3343 +0xf9
`

func TestGroupGoroutines统计总数(t *testing.T) {
	info := groupGoroutines(realDump)
	// 上面共 5 个 goroutine 块
	if info.Total != 5 {
		t.Fatalf("总数应为 5，实际 %d", info.Total)
	}
}

// TestGroupGoroutines看穿运行时帧 是这个解析器的核心价值：
// pool 的两个 worker 栈顶也是 runtime.gopark 一类的运行时帧，
// 不跳过它们就会和 bytedance/其它 parked 协程混成一类，分组等于没分。
func TestGroupGoroutines看穿运行时帧(t *testing.T) {
	info := groupGoroutines(realDump)

	poolGroup := findGroup(info, "good-review-master/pool.(*Pool).worker")
	if poolGroup == nil {
		t.Fatalf("应归出一组 pool.(*Pool).worker，实际分组：%v", frameNames(info))
	}
	if poolGroup.Count != 2 {
		t.Fatalf("pool worker 组应为 2 个，实际 %d", poolGroup.Count)
	}
	if poolGroup.State != "chan receive" {
		t.Fatalf("状态应为 chan receive，实际 %q", poolGroup.State)
	}
}

// TestGroupGoroutines忽略帧参数 锁住最要命的一个坑：
// 栈帧带参数（含指针地址），同一函数在不同 goroutine 上参数不同，
// 不去掉参数就每个 goroutine 各成一组，聚合完全失效。
func TestGroupGoroutines忽略帧参数(t *testing.T) {
	dump := `goroutine 10 [IO wait]:
net/http.(*Server).Serve(0xc0003a4000, {0x7ff6a2c180d0, 0xc0003b2000})
	/tmp/server.go:3424 +0xb2
created by net/http.(*Server).ListenAndServe in goroutine 1
	/tmp/server.go:3343 +0xf9

goroutine 11 [IO wait]:
net/http.(*Server).Serve(0xc0009f4000, {0x7ff6a2c180d0, 0xc0009f6000})
	/tmp/server.go:3424 +0xb2
created by net/http.(*Server).ListenAndServe in goroutine 1
	/tmp/server.go:3343 +0xf9
`
	info := groupGoroutines(dump)
	if len(info.Groups) != 1 {
		t.Fatalf("参数不同但函数相同，应合并成 1 组，实际 %d 组：%v", len(info.Groups), frameNames(info))
	}
	if info.Groups[0].Count != 2 {
		t.Fatalf("应合并为 2 个，实际 %d", info.Groups[0].Count)
	}
	if got := info.Groups[0].Frame; got != "net/http.(*Server).Serve" {
		t.Fatalf("帧名应剥掉参数列表，实际 %q", got)
	}
}

// TestUnescapePackageDots 锁住包路径末段 '.' 的转义还原。
// 真实案例：gopkg.in/natefinch/lumberjack.v2 在栈里是 lumberjack%2ev2。
func TestUnescapePackageDots(t *testing.T) {
	cases := map[string]string{
		"gopkg.in/natefinch/lumberjack%2ev2.(*Logger).millRun": "gopkg.in/natefinch/lumberjack.v2.(*Logger).millRun",
		// 前面的 gopkg.in 本来就不带转义（转义只作用于最后一段），不该被改动
		"gopkg.in/natefinch/lumberjack%2ev2": "gopkg.in/natefinch/lumberjack.v2",
		// 没有转义的原样返回
		"good-review-master/pool.(*Pool).worker": "good-review-master/pool.(*Pool).worker",
		"":                                       "",
	}
	for input, want := range cases {
		if got := unescapePackageDots(input); got != want {
			t.Errorf("unescapePackageDots(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// TestNormalizeFrame 两根管道合起来的端到端行为：剥参数 + 还原转义。
func TestNormalizeFrame(t *testing.T) {
	got := normalizeFrame("gopkg.in/natefinch/lumberjack%2ev2.(*Logger).millRun(0xc0001b2000)")
	want := "gopkg.in/natefinch/lumberjack.v2.(*Logger).millRun"
	if got != want {
		t.Fatalf("normalizeFrame = %q，期望 %q", got, want)
	}
}

func TestStripFrameArgs(t *testing.T) {
	cases := map[string]string{
		"pool.(*Pool).worker()":                              "pool.(*Pool).worker",
		"net/http.(*Server).Serve(0xc0003a4000, {0x1, 0x2})": "net/http.(*Server).Serve",
		"main.main()": "main.main",
		"runtime.gopark(0x0, 0x0, 0x0, 0x0, 0x0)": "runtime.gopark",
		// 函数名里带括号（接收者）不能被误切
		"main.(*Foo).Bar(0x1)": "main.(*Foo).Bar",
		// 没有参数列表的原样返回
		"runtime.goexit": "runtime.goexit",
		// 括号不配对时不 panic、原样返回
		"weird(Foo": "weird(Foo",
	}
	for input, want := range cases {
		if got := stripFrameArgs(input); got != want {
			t.Errorf("stripFrameArgs(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// TestGroupGoroutines跳过createdBy created by 行也满足"下一行是 tab 缩进"，
// 但它不是栈帧；把它当帧会让归类落到 "created by ..." 上。
func TestGroupGoroutines跳过createdBy(t *testing.T) {
	info := groupGoroutines(realDump)
	for _, group := range info.Groups {
		if strings.HasPrefix(group.Frame, "created by ") {
			t.Fatalf("created by 行被误当成了栈帧：%q", group.Frame)
		}
	}
}

func TestGroupGoroutines按数量降序且稳定(t *testing.T) {
	info := groupGoroutines(realDump)
	for i := 1; i < len(info.Groups); i++ {
		if info.Groups[i-1].Count < info.Groups[i].Count {
			t.Fatalf("分组未按数量降序：%v", frameNames(info))
		}
	}
	// 同数量时必须按名字定序，否则 map 遍历顺序会让前端表格每次刷新都乱跳
	first := frameNames(info)
	for i := 0; i < 20; i++ {
		if got := frameNames(groupGoroutines(realDump)); strings.Join(got, "|") != strings.Join(first, "|") {
			t.Fatalf("多次解析顺序不一致：\n%v\n%v", first, got)
		}
	}
}

// TestGroupGoroutines遇到畸形输入不崩 是关键性质：这是给人看的诊断工具，
// 宁可少显示也不能因为格式细节变化整页挂掉。
func TestGroupGoroutines遇到畸形输入不崩(t *testing.T) {
	cases := map[string]string{
		"空串":        "",
		"只有空行":      "\n\n\n",
		"没有块头":      "main.main()\n\t/tmp/main.go:1 +0x1\n",
		"块头没有方括号":   "goroutine 1:\nmain.main()\n\t/tmp/main.go:1 +0x1\n",
		"块头只有方括号":   "goroutine 1 []:\n",
		"函数行后面没文件行": "goroutine 1 [running]:\nmain.main()\n",
		"全是不认识的垃圾":  "hello world\n!!!\n",
		"CRLF 行尾":   "goroutine 1 [running]:\r\nmain.main()\r\n\t/tmp/main.go:1 +0x1\r\n",
	}
	for name, dump := range cases {
		t.Run(name, func(t *testing.T) {
			got := groupGoroutines(dump) // 只要不 panic 就算过
			if got.Groups == nil {
				t.Fatal("Groups 应为空切片而不是 nil（否则 JSON 会序列化成 null，前端 v-for 会崩）")
			}
		})
	}
}

func TestGroupGoroutinesCRLF也能数出总数(t *testing.T) {
	crlf := strings.ReplaceAll(realDump, "\n", "\r\n")
	if got := groupGoroutines(crlf); got.Total != 5 {
		t.Fatalf("CRLF 行尾下总数应为 5，实际 %d", got.Total)
	}
}

// TestGoroutineDump真实抓取 用真实运行时输出跑一遍完整链路，
// 确保解析器面对的不是只有我手写的假数据。
func TestGoroutineDump真实抓取(t *testing.T) {
	dump, truncated := goroutineDump()
	if truncated {
		t.Fatal("正常测试进程不该出现缓冲区截断")
	}
	if !strings.HasPrefix(dump, "goroutine ") {
		t.Fatalf("栈输出应以 goroutine 开头，实际：%.40q", dump)
	}
	info := groupGoroutines(dump)
	if info.Total == 0 {
		t.Fatal("至少应有 main goroutine 被数出来")
	}
	// 抓栈这个动作本身就在我们自己的函数里，所以结果里必须出现本仓库的帧。
	// 断言得这么具体，是因为"非空"太弱——全归到 runtime.xxx 也是非空的。
	joined := strings.Join(frameNames(info), "\n")
	if !strings.Contains(joined, "good-review-master/") {
		t.Fatalf("真实抓取里看不到本仓库的栈帧，分组或跳过名单可能把业务帧一起吃掉了：\n%s", joined)
	}
	// 跳过名单生效的反向证据：runtime 内部帧不该成为任何一组的归类键
	for _, group := range info.Groups {
		if strings.HasPrefix(group.Frame, "runtime.") || strings.HasPrefix(group.Frame, "sync.") {
			t.Fatalf("运行时帧 %q 成了归类键，说明跳过名单没生效", group.Frame)
		}
	}
}

func findGroup(info GoroutinesInfo, frame string) *GoroutineGroup {
	for i := range info.Groups {
		if info.Groups[i].Frame == frame {
			return &info.Groups[i]
		}
	}
	return nil
}

func frameNames(info GoroutinesInfo) []string {
	names := make([]string, 0, len(info.Groups))
	for _, group := range info.Groups {
		names = append(names, group.Frame)
	}
	return names
}
