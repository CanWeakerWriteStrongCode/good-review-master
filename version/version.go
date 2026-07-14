package version

import "fmt"

// 构建时由 -ldflags 注入，默认值在 go run 时生效
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// String 返回格式化版本信息
func String() string {
	return fmt.Sprintf("%s (commit: %s, built: %s)", Version, Commit, BuildTime)
}
