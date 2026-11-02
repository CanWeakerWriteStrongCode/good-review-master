package server

import (
	"embed"
	"os"
	"path/filepath"

	"good-review-master/logutil"
)

// localeFiles 是语言文件的出厂版本，只在"磁盘上还没有这份文件"时用一次。
//
// 之后一律以磁盘那份为准：改文案不用重新编译，也不用去代码里找文案在哪。
// 这也意味着**修改模板要同时改这里的源文件和磁盘上的副本**才算完整——
// 但实际使用中没人会去重启到一个空目录，所以这条只在首次部署时走。
//
//go:embed locales/*.yaml
var localeFiles embed.FS

// ensureLocaleFiles 首跑时把内嵌的语言文件写到 dir 下，已存在的不覆盖。
func ensureLocaleFiles(dir string) {
	entries, err := localeFiles.ReadDir("locales")
	if err != nil {
		logutil.Error("读取内嵌语言文件失败", "err", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if _, err := os.Stat(path); err == nil {
			// 已经在磁盘上了：可能是用户改过的版本，不能覆盖
			continue
		}
		data, err := localeFiles.ReadFile("locales/" + entry.Name())
		if err != nil {
			logutil.Error("读取内嵌语言文件失败", "file", entry.Name(), "err", err)
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			logutil.Error("创建语言目录失败", "dir", dir, "err", err)
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			logutil.Error("写出语言文件失败", "path", path, "err", err)
			continue
		}
		logutil.Info("已生成语言文件", "path", path)
	}
}
