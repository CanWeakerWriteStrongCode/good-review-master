package server

import (
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/gin-gonic/gin"
)

// registerPprof 把标准库 net/http/pprof 挂到**已鉴权**的 /api/debug/pprof/ 下。
//
// 为什么值得开：本项目自研了协程池（pool/async），goroutine 泄漏不会自己暴露，
// 只会表现为内存与句柄缓慢上涨；/debug/pprof/goroutine 是唯一能一眼看清"谁在泄漏"的手段。
//
// 三条刻意的取舍：
//  1. **不用默认的 http.DefaultServeMux**（那会让 /debug/pprof/ 以完全无鉴权的方式暴露），
//     而是显式注册到 apiGroup 下 —— pprof 能 dump 出堆内存与调用栈（含配置内容），
//     当作公开端点是信息泄露。
//  2. 只注册**一条 catch-all**，在 handlePprof 里手动分派，规则与 DefaultServeMux 一致。
//     不能逐个注册 /pprof/heap、/pprof/cmdline、/pprof/:name：gin 的路由树不允许
//     同一层级同时存在静态段与通配段，那样写会在启动时直接 panic。
//  3. 由 runtime.enable_pprof 控制，默认关闭：它本身有不小开销
//     （CPU profile 会采样、heap profile 会触发 GC），不该常开。
func registerPprof(group *gin.RouterGroup) {
	group.GET("/pprof/*profile", handlePprof)
}

// handlePprof 按 net/http/pprof 的命名规则分派到对应的标准库 handler。
func handlePprof(c *gin.Context) {
	name := strings.TrimPrefix(c.Param("profile"), "/")

	var profileHandler http.Handler
	switch name {
	case "": // /pprof/ 索引页
		profileHandler = http.HandlerFunc(pprof.Index)
	case "cmdline":
		profileHandler = http.HandlerFunc(pprof.Cmdline)
	case "profile": // CPU profile，?seconds=N
		profileHandler = http.HandlerFunc(pprof.Profile)
	case "symbol":
		profileHandler = http.HandlerFunc(pprof.Symbol)
	case "trace": // 执行 trace，?seconds=N
		profileHandler = http.HandlerFunc(pprof.Trace)
	default:
		// 命名 profile：goroutine / heap / allocs / block / mutex / threadcreate。
		// 未知名字时 Handler 自己会回 404 "Unknown profile"，无需在这里拦。
		profileHandler = pprof.Handler(name)
	}
	profileHandler.ServeHTTP(c.Writer, c.Request)
}
