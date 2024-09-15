//go:generate mockgen -destination=mocks/logger.go -package=gocronmocks . Logger
package cron

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// Logger 是包装 gocron 使用的基本日志记录方法的接口。
// 这些方法以标准库 slog 包为模型。
// 默认 Logger 是 no-op Logger。
// 要启用日志记录，请使用提供的 NewLogger 函数之一或实现您自己的 Logger。
// 记录的实际 Log 级别由实现处理。
type Logger interface {
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

var _ Logger = (*noOpLogger)(nil)

type noOpLogger struct{}

func (l noOpLogger) Debug(_ string, _ ...any) {}
func (l noOpLogger) Error(_ string, _ ...any) {}
func (l noOpLogger) Info(_ string, _ ...any)  {}
func (l noOpLogger) Warn(_ string, _ ...any)  {}

var _ Logger = (*logger)(nil)

// LogLevel is the level of logging that should be logged
// when using the basic NewLogger.
type LogLevel int

// The different log levels that can be used.
const (
	LogLevelError LogLevel = iota
	LogLevelWarn
	LogLevelInfo
	LogLevelDebug
)

type logger struct {
	log   *log.Logger
	level LogLevel
}

// NewLogger 返回一个在给定级别记录的新 Logger。
func NewLogger(level LogLevel) Logger {
	l := log.New(os.Stdout, "", log.LstdFlags)
	return &logger{
		log:   l,
		level: level,
	}
}

func (l *logger) Debug(msg string, args ...any) {
	if l.level < LogLevelDebug {
		return
	}
	l.log.Printf("DEBUG: %s%s\n", msg, logFormatArgs(args...))
}

func (l *logger) Error(msg string, args ...any) {
	if l.level < LogLevelError {
		return
	}
	l.log.Printf("ERROR: %s%s\n", msg, logFormatArgs(args...))
}

func (l *logger) Info(msg string, args ...any) {
	if l.level < LogLevelInfo {
		return
	}
	l.log.Printf("INFO: %s%s\n", msg, logFormatArgs(args...))
}

func (l *logger) Warn(msg string, args ...any) {
	if l.level < LogLevelWarn {
		return
	}
	l.log.Printf("WARN: %s%s\n", msg, logFormatArgs(args...))
}

func logFormatArgs(args ...any) string {
	if len(args) == 0 {
		return ""
	}
	if len(args)%2 != 0 {
		return ", " + fmt.Sprint(args...)
	}
	var pairs []string
	for i := 0; i < len(args); i += 2 {
		pairs = append(pairs, fmt.Sprintf("%s=%v", args[i], args[i+1]))
	}
	return ", " + strings.Join(pairs, ", ")
}
