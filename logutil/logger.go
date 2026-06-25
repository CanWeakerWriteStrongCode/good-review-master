package logutil

import (
	"os"
	"path/filepath"
	"time"

	"good-review-master/apppath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var sugar *zap.SugaredLogger

// SetupLogger 初始化 zap 日志：控制台 + 文件（lumberjack 按大小切割，保留 30 天，压缩旧文件）
func SetupLogger() {
	logDir := filepath.Join(apppath.ExeDir(), "log")
	os.MkdirAll(logDir, 0755)

	// 文件输出：lumberjack 自动切割
	fileWriter := zapcore.AddSync(&lumberjack.Logger{
		Filename:   filepath.Join(logDir, "bot.log"),
		MaxSize:    20,   // MB
		MaxBackups: 30,   // 最多保留 30 个旧文件
		MaxAge:     30,   // 最多保留 30 天
		Compress:   true, // 旧文件 gzip 压缩
	})

	// 控制台输出
	consoleWriter := zapcore.AddSync(os.Stdout)

	encoderConfig := zapcore.EncoderConfig{
		TimeKey:       "time",
		LevelKey:      "level",
		NameKey:       "logger",
		CallerKey:     "caller",
		MessageKey:    "msg",
		StacktraceKey: "stacktrace",
		LineEnding:    zapcore.DefaultLineEnding,
		EncodeLevel:   zapcore.CapitalColorLevelEncoder,
		EncodeTime: func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
			enc.AppendString(t.Format("2006-01-02 15:04:05"))
		},
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// 双输出：控制台用 console 编码，文件用 console 编码（可读性好）
	consoleCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderConfig),
		consoleWriter,
		zap.InfoLevel,
	)
	fileCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderConfig),
		fileWriter,
		zap.InfoLevel,
	)

	base := zap.New(zapcore.NewTee(consoleCore, fileCore), zap.AddCaller())
	sugar = base.Sugar()
	zap.ReplaceGlobals(base)
}

// Info 输出 Info 级别日志
func Info(msg string, keysAndValues ...interface{}) {
	sugar.Infow(msg, keysAndValues...)
}

// Error 输出 Error 级别日志
func Error(msg string, keysAndValues ...interface{}) {
	sugar.Errorw(msg, keysAndValues...)
}

// Warn 输出 Warn 级别日志
func Warn(msg string, keysAndValues ...interface{}) {
	sugar.Warnw(msg, keysAndValues...)
}

// Debug 输出 Debug 级别日志
func Debug(msg string, keysAndValues ...interface{}) {
	sugar.Debugw(msg, keysAndValues...)
}
