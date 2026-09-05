package logutil

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"good-review-master/apppath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var sugar *zap.SugaredLogger
var lumberjackLogger *lumberjack.Logger

// SetupLogger 初始化 zap 日志：控制台 + 文件（lumberjack 按大小切割，保留 30 天，压缩旧文件）
func SetupLogger() {
	dir := apppath.ExeDir()
	logDir := filepath.Join(dir, "log")
	os.MkdirAll(logDir, 0755)

	// 文件输出：lumberjack 自动切割
	lumberjackLogger = &lumberjack.Logger{
		Filename:   filepath.Join(logDir, "bot.log"),
		MaxSize:    20,   // MB
		MaxBackups: 30,   // 最多保留 30 个旧文件
		MaxAge:     30,   // 最多保留 30 天
		Compress:   true, // 旧文件 gzip 压缩
	}
	fileWriter := zapcore.AddSync(lumberjackLogger)

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

	// 双输出：控制台用 console 编码，文件用 console 编码（可读性好）。
	// 级别默认 Info；设 GOOD_REVIEW_LOG_LEVEL=debug 可放开 Debug 明细
	// （MCP 工具调用/选窗决策等大量 logutil.Debug 平时会被 InfoLevel 过滤掉）。
	level := logLevelFromEnv()
	consoleCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderConfig),
		consoleWriter,
		level,
	)
	fileCore := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encoderConfig),
		fileWriter,
		level,
	)

	base := zap.New(zapcore.NewTee(consoleCore, fileCore), zap.AddCaller())
	// AddCallerSkip(1) 跳过 logutil 包装函数帧，让 caller 指向真实调用位置
	sugar = base.WithOptions(zap.AddCallerSkip(1)).Sugar()
	zap.ReplaceGlobals(base)
}

// logLevelFromEnv 从 GOOD_REVIEW_LOG_LEVEL 读日志级别：debug|info|warn|error，非法值回退 info
func logLevelFromEnv() zapcore.Level {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GOOD_REVIEW_LOG_LEVEL"))) {
	case "debug", "d":
		return zapcore.DebugLevel
	case "warn", "warning", "w":
		return zapcore.WarnLevel
	case "error", "e":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// Close 刷新并关闭日志文件（释放句柄；测试收尾或优雅停机时调用）
func Close() {
	if sugar != nil {
		_ = sugar.Sync()
	}
	if lumberjackLogger != nil {
		_ = lumberjackLogger.Close()
	}
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
