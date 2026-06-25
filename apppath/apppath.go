// Package apppath 提供应用路径解析工具函数。
// 可执行文件目录优先作为读写目标，工作目录作为回退。
package apppath

import (
	"os"
	"path/filepath"
)

// ExeDir 返回可执行文件所在目录，获取失败时返回当前目录
func ExeDir() string {
	if exePath, err := os.Executable(); err == nil {
		return filepath.Dir(exePath)
	}
	return "."
}

// ResolvePath 查找文件路径：优先工作目录，其次 exe 所在目录
func ResolvePath(filename string) string {
	for _, dir := range []string{".", ExeDir()} {
		path := filepath.Join(dir, filename)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filename
}

// WritePath 返回写文件的优先路径：exe 所在目录，不可用时回退到当前目录
func WritePath(filename string) string {
	dir := ExeDir()
	if _, err := os.Stat(dir); err != nil {
		return filename
	}
	return filepath.Join(dir, filename)
}
