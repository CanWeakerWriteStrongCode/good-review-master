package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"

	"github.com/gin-gonic/gin"
)

// isolateWorkdir 把进程 cwd 切到临时目录，测试结束再还原。
//
// 必须这么做：logutil.SetupLogger 会在 apppath.ExeDir() 下建 log/ 写日志，
// 而 ExeDir() 优先返回 cwd——go test 的 cwd 就是包目录，于是跑一次测试
// 就在 web/server/ 里留下一份日志文件。
// 用 cwd 隔离而不是改 logutil：测试的卫生问题不该让生产代码迁就。
func isolateWorkdir(t *testing.T) {
	t.Helper()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("取当前工作目录失败：%v", err)
	}

	// 注册顺序有讲究：t.Cleanup 是后进先出，而 t.TempDir() 的"删目录"清理
	// 是这里最先注册、最后执行的。所以 logutil.Close 必须注册在它**之后**，
	// 才能赶在删目录之前跑——Windows 不允许删除仍被打开的 bot.log。
	temp := t.TempDir()
	t.Cleanup(func() {
		logutil.Close()
		_ = os.Chdir(original)
	})

	if err := os.Chdir(temp); err != nil {
		t.Fatalf("切换工作目录失败：%v", err)
	}
}

// TestShutdown等待期间readyz转503且仍接受新连接 锁定 pre-stop 等待这个机制本身。
//
// 这条测试的存在理由很具体：http.Server.Shutdown 会**立即关闭监听器**，
// 所以「置 draining → 直接调 Shutdown」时，探针根本建立不了新连接，
// 503 没有任何观察窗口（实测连接会被强行关闭）。
// 只有中间插入等待，调用方才有机会看到 503 并把流量摘走。
func TestShutdown等待期间readyz转503且仍接受新连接(t *testing.T) {
	isolateWorkdir(t)
	logutil.SetupLogger()

	const shutdownDelay = 1 * time.Second
	port := freeTCPPort(t)
	server := New(&config.Config{
		WebPort:       port,
		WebUsername:   "admin",
		WebPassword:   "pw",
		JWTSecret:     "test-secret",
		ShutdownDelay: shutdownDelay,
	}, onebot.NewClient("http://127.0.0.1:1", ""))

	go func() { _ = server.Start() }()
	waitForServer(t, port)

	if got := getStatus(t, port, "/readyz"); got != http.StatusOK {
		t.Fatalf("关闭前 /readyz 应为 200，实际 %d", got)
	}

	shutdownReturned := make(chan struct{})
	go func() {
		defer close(shutdownReturned)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	// 等待期内：探针看到 503（这一步本身就证明了监听器还开着，否则连不上）
	waitForStatus(t, port, "/readyz", http.StatusServiceUnavailable)
	// 存活探针不受影响：进程没死，只是不接新活了
	if got := getStatus(t, port, "/healthz"); got != http.StatusOK {
		t.Fatalf("等待期内 /healthz 应仍为 200，实际 %d", got)
	}
	// 等待期还没走完，Shutdown 不该已经返回
	select {
	case <-shutdownReturned:
		t.Fatal("等待期未结束 Shutdown 就返回了，pre-stop 等待没生效")
	default:
	}

	<-shutdownReturned
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond); err == nil {
		t.Fatal("Shutdown 返回后端口仍在监听")
	}
}

// TestShutdown默认等到在途请求收尾 锁定不配 shutdown_delay_sec 时的默认行为：
// 不停留等待（不为没有观察者的部署拖慢退出），但**在途请求仍会被等到跑完**，
// 不会被中途砍断。
func TestShutdown默认等到在途请求收尾(t *testing.T) {
	isolateWorkdir(t)
	logutil.SetupLogger()

	port := freeTCPPort(t)
	server := New(&config.Config{
		WebPort:     port,
		WebUsername: "admin",
		WebPassword: "pw",
		JWTSecret:   "test-secret",
		// 不设 ShutdownDelay：默认 0
	}, onebot.NewClient("http://127.0.0.1:1", ""))

	// 测试专用路由：卡在 handler 里直到测试放行，用来制造在途请求。
	// 用 channel 协调而不是 sleep，时序是确定的。
	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	server.engine.GET("/__slow", func(c *gin.Context) {
		close(handlerEntered)
		<-releaseHandler
		c.String(http.StatusOK, "done")
	})

	go func() { _ = server.Start() }()
	waitForServer(t, port)

	slowDone := make(chan struct{})
	go func() {
		defer close(slowDone)
		resp, err := http.Get(baseURL(port) + "/__slow")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	<-handlerEntered // 请求确实在途了

	shutdownReturned := make(chan struct{})
	go func() {
		defer close(shutdownReturned)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	select {
	case <-shutdownReturned:
		t.Fatal("在途请求未结束时 Shutdown 就返回了，说明没有等待在途请求收尾")
	case <-time.After(200 * time.Millisecond):
		// 预期：Shutdown 正卡在等在途请求
	}

	close(releaseHandler)
	<-slowDone
	<-shutdownReturned

	if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond); err == nil {
		t.Fatal("Shutdown 返回后端口仍在监听")
	}
}

func baseURL(port int) string { return fmt.Sprintf("http://127.0.0.1:%d", port) }

// freeTCPPort 借一个空闲端口：先监听 :0 拿到系统分配的端口再释放。
func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请空闲端口失败：%v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func waitForServer(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			// 必须关掉这个探测连接：它只连上、没发请求，服务端记的是 StateNew。
			// http.Server.Shutdown 对 StateNew 连接有 5 秒宽限（golang/go#22682，
			// 怕误杀刚握手还没读到首字节的正常客户端），留着它会让每次 Shutdown
			// 都白等 5 秒——测试里就表现为"关闭耗时 5.8s"。
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("端口 %d 在 5 秒内没有开始监听", port)
}

func getStatus(t *testing.T, port int, path string) int {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(baseURL(port) + path)
	if err != nil {
		t.Fatalf("请求 %s 失败：%v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func waitForStatus(t *testing.T, port int, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		last = getStatus(t, port, path)
		if last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 在 5 秒内没有变成 %d，最后一次是 %d", path, want, last)
}
