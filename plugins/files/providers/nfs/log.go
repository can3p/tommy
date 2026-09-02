package nfs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	gonfs "github.com/willscott/go-nfs"
)

// go-nfs logs through a package-level global (nfs.Log) that writes to the
// standard log package at info level, which in a tommy process means RPC
// chatter on stderr in a format nothing else here uses. There is no per-server
// logger to set, so the global is replaced - once, because it is process-wide
// state and two files providers in one binary must not fight over it.
var installOnce sync.Once

func installLogger(logger *slog.Logger) {
	if logger == nil {
		return
	}
	installOnce.Do(func() {
		gonfs.SetLogger(&slogLogger{log: logger, level: gonfs.InfoLevel})
	})
}

// slogLogger bridges go-nfs's Logger onto slog. The mapping is deliberately
// pessimistic about severity: go-nfs calls Errorf for perfectly ordinary
// client-side outcomes - a lookup that misses, a create of something that
// already exists - so its errors are logged at debug and its warnings at
// info. Anything louder would make a working server look broken.
//
// Panic and Fatal are not fatal here either: go-nfs's own default logger only
// prints them, so a library that logs one must not take a tommy process down
// with it.
type slogLogger struct {
	log *slog.Logger

	mu    sync.RWMutex
	level gonfs.LogLevel
}

var _ gonfs.Logger = (*slogLogger)(nil)

func (l *slogLogger) SetLevel(level gonfs.LogLevel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

func (l *slogLogger) GetLevel() gonfs.LogLevel {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.level
}

func (l *slogLogger) ParseLevel(level string) (gonfs.LogLevel, error) {
	switch level {
	case "panic":
		return gonfs.PanicLevel, nil
	case "fatal":
		return gonfs.FatalLevel, nil
	case "error":
		return gonfs.ErrorLevel, nil
	case "warn":
		return gonfs.WarnLevel, nil
	case "info":
		return gonfs.InfoLevel, nil
	case "debug":
		return gonfs.DebugLevel, nil
	case "trace":
		return gonfs.TraceLevel, nil
	}
	var zero gonfs.LogLevel
	return zero, fmt.Errorf("nfs: invalid log level %q", level)
}

// enabled reports whether the library's own level gate lets a message
// through, so LOG_LEVEL and SetLevel keep working the way go-nfs documents.
func (l *slogLogger) enabled(at gonfs.LogLevel) bool { return l.GetLevel() >= at }

func (l *slogLogger) emit(at gonfs.LogLevel, to slog.Level, msg string) {
	if !l.enabled(at) {
		return
	}
	l.log.Log(context.Background(), to, msg)
}

func (l *slogLogger) Panic(args ...any) {
	l.emit(gonfs.PanicLevel, slog.LevelError, fmt.Sprint(args...))
}
func (l *slogLogger) Fatal(args ...any) {
	l.emit(gonfs.FatalLevel, slog.LevelError, fmt.Sprint(args...))
}
func (l *slogLogger) Error(args ...any) {
	l.emit(gonfs.ErrorLevel, slog.LevelDebug, fmt.Sprint(args...))
}
func (l *slogLogger) Warn(args ...any) { l.emit(gonfs.WarnLevel, slog.LevelInfo, fmt.Sprint(args...)) }
func (l *slogLogger) Info(args ...any) { l.emit(gonfs.InfoLevel, slog.LevelDebug, fmt.Sprint(args...)) }
func (l *slogLogger) Debug(args ...any) {
	l.emit(gonfs.DebugLevel, slog.LevelDebug, fmt.Sprint(args...))
}
func (l *slogLogger) Trace(args ...any) {
	l.emit(gonfs.TraceLevel, slog.LevelDebug, fmt.Sprint(args...))
}
func (l *slogLogger) Print(args ...any) {
	l.emit(gonfs.InfoLevel, slog.LevelDebug, fmt.Sprint(args...))
}

func (l *slogLogger) Panicf(format string, args ...any) {
	l.emit(gonfs.PanicLevel, slog.LevelError, fmt.Sprintf(format, args...))
}

func (l *slogLogger) Fatalf(format string, args ...any) {
	l.emit(gonfs.FatalLevel, slog.LevelError, fmt.Sprintf(format, args...))
}

func (l *slogLogger) Errorf(format string, args ...any) {
	l.emit(gonfs.ErrorLevel, slog.LevelDebug, fmt.Sprintf(format, args...))
}

func (l *slogLogger) Warnf(format string, args ...any) {
	l.emit(gonfs.WarnLevel, slog.LevelInfo, fmt.Sprintf(format, args...))
}

func (l *slogLogger) Infof(format string, args ...any) {
	l.emit(gonfs.InfoLevel, slog.LevelDebug, fmt.Sprintf(format, args...))
}

func (l *slogLogger) Debugf(format string, args ...any) {
	l.emit(gonfs.DebugLevel, slog.LevelDebug, fmt.Sprintf(format, args...))
}

func (l *slogLogger) Tracef(format string, args ...any) {
	l.emit(gonfs.TraceLevel, slog.LevelDebug, fmt.Sprintf(format, args...))
}

func (l *slogLogger) Printf(format string, args ...any) {
	l.emit(gonfs.InfoLevel, slog.LevelDebug, fmt.Sprintf(format, args...))
}
