// Package apppath 提供应用路径解析工具函数。
// 工作目录优先作为读写目标，可执行文件目录作为回退。
package apppath

import (
	"os"
	"path/filepath"
)

// ExeDir 返回当前工作目录，获取失败时回退到可执行文件所在目录
func ExeDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	if exePath, err := os.Executable(); err == nil {
		return filepath.Dir(exePath)
	}
	return "."
}

// ResolvePath 查找文件路径：ExeDir（cwd 优先 → exeDir 回退），用 os.Stat 确认存在
func ResolvePath(filename string) string {
	path := filepath.Join(ExeDir(), filename)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	return filename
}

// WritePath 返回写文件的路径：ExeDir 目录存在则写入其下，否则回退到当前目录
func WritePath(filename string) string {
	dir := ExeDir()
	if _, err := os.Stat(dir); err != nil {
		return filename
	}
	return filepath.Join(dir, filename)
}
