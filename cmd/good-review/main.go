package main

import (
	"fmt"
	"os"

	"good-review-master/app"
	"good-review-master/apppath"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/version"
)

// main 只保留三件不属于装配的事：日志初始化、首跑提示、把错误翻译成进程退出码。
// 组件如何构造、如何接线、如何按序启停，全部在 app 包（组合根）里。
func main() {
	logutil.SetupLogger()
	logutil.Info("版本：" + version.String())

	// 首跑：补全模板配置文件，提示用户配置后再启动
	if config.InitDefaultFiles() {
		fmt.Println("已创建 config.yaml 文件，请配置 config.yaml 后再次启动程序，按回车键退出...")
		fmt.Scanln()
		os.Exit(0)
	}

	application, err := app.New(app.Options{
		ConfigPath:       apppath.ResolvePath("config.yaml"),
		SecretPath:       apppath.ResolvePath("secret.yaml"),
		SystemPromptPath: apppath.ResolvePath("prompt_system.yaml"),
		TestMode:         os.Getenv("GOOD_REVIEW_TEST") == "1",
	})
	if err != nil {
		// 配置不合法的具体原因（哪一段、当前值、该怎么改）已经在 config 包里写清楚了，
		// 这里不再按错误类型分派不同文案——那种分派会随着校验规则增加而不断膨胀。
		logutil.Error(err.Error())
		os.Exit(1)
	}

	// 阻塞运行，直到收到退出信号并完成优雅关闭
	application.Run()
}
