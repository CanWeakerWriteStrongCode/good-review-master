package logutil

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	maxSize = 20 * 1024 * 1024 // 20MB
	maxDays = 30
)

type dailyWriter struct {
	mu       sync.Mutex
	dir      string
	file     *os.File
	curDate  string
	curSize  int64
	curIndex int
}

// SetupLogger 初始化日志：exe 同级 log/ 目录，按天滚动，20MB 切片，30 天清理，同时输出控制台
func SetupLogger() {
	logDir := filepath.Join(exeDir(), "log")
	os.MkdirAll(logDir, 0755)

	writer := &dailyWriter{dir: logDir}

	handler := slog.NewTextHandler(io.MultiWriter(os.Stdout, writer), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(handler))
}

func exeDir() string {
	if exePath, err := os.Executable(); err == nil {
		return filepath.Dir(exePath)
	}
	return "."
}

func (dw *dailyWriter) Write(data []byte) (written int, err error) {
	dw.mu.Lock()
	defer dw.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	if dw.file == nil || today != dw.curDate || dw.curSize >= maxSize {
		if err := dw.rotate(today); err != nil {
			return 0, err
		}
	}

	written, err = dw.file.Write(data)
	dw.curSize += int64(written)
	return
}

func (dw *dailyWriter) rotate(today string) error {
	if dw.file != nil {
		dw.file.Close()
	}

	if today != dw.curDate {
		dw.curIndex = 0
		dw.curDate = today
		go dw.cleanOld()
	} else {
		dw.curIndex++
	}

	var name string
	if dw.curIndex == 0 {
		name = today + ".log"
	} else {
		name = fmt.Sprintf("%s_%d.log", today, dw.curIndex)
	}

	logFile, err := os.OpenFile(filepath.Join(dw.dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	info, _ := logFile.Stat()
	dw.file = logFile
	dw.curSize = info.Size()
	return nil
}

func (dw *dailyWriter) cleanOld() {
	cutoff := time.Now().AddDate(0, 0, -maxDays)
	entries, _ := os.ReadDir(dw.dir)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if len(name) < 10 {
			continue
		}
		fileDate, err := time.Parse("2006-01-02", name[:10])
		if err != nil {
			continue
		}
		if fileDate.Before(cutoff) {
			os.Remove(filepath.Join(dw.dir, name))
		}
	}
}
