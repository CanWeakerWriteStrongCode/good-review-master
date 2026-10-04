package llm

import (
	"os"
	"testing"

	"good-review-master/logutil"
)

// TestMain 初始化 logger 并隔离工作目录。
//
// 熔断器的状态变化回调会打日志，而 logutil 未初始化时 sugar 是 nil——
// 一调就 panic（本仓库里这个坑已经撞过好几次）。同时 SetupLogger 会在 cwd 下建 log/，
// 不隔离就会把 bot.log 写进源码树。
func TestMain(m *testing.M) {
	original, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	temp, err := os.MkdirTemp("", "llm-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(temp); err != nil {
		panic(err)
	}
	logutil.SetupLogger()

	code := m.Run()

	logutil.Close()
	_ = os.Chdir(original)
	_ = os.RemoveAll(temp)
	os.Exit(code)
}
