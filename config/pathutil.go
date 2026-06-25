package config

import (
	"os"
	"path/filepath"
)

// exeDir 返回可执行文件所在目录
func exeDir() string {
	if exePath, err := os.Executable(); err == nil {
		return filepath.Dir(exePath)
	}
	return "."
}

// resolveConfigPath 查找配置文件路径：优先工作目录，其次 exe 所在目录
func resolveConfigPath(filename string) string {
	for _, dir := range []string{".", exeDir()} {
		path := filepath.Join(dir, filename)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filename
}

// writePath 返回写文件的优先路径：exe 所在目录，不可用时回退到当前目录
func writePath(filename string) string {
	dir := exeDir()
	if _, err := os.Stat(dir); err != nil {
		return filename
	}
	return filepath.Join(dir, filename)
}
