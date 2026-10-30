// Package logging provides a zap-based structured logger with context injection.
package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/mo-cmyk/mazarbul/internal/config"
)

// Format selects the encoder shape.
type Format int

const (
	// FormatJSON emits one JSON object per line. Preferred for production.
	FormatJSON Format = iota
	// FormatText emits human-readable console output. Preferred for dev.
	FormatText
)

// Level selects the minimum log level.
type Level int

// Log levels in increasing severity.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Default rotation knobs applied when FilePath is set and the corresponding
// Config field is zero.
const (
	defaultMaxSizeMB  = 10
	defaultMaxBackups = 5
)

// Config configures a Logger.
type Config struct {
	// Output is where log entries are written when FilePath is empty.
	// Defaults to os.Stderr. Ignored when FilePath is non-empty.
	Output io.Writer
	// Level is the minimum severity to emit. Defaults to LevelInfo.
	Level Level
	// Format is JSON or text. Defaults to JSON.
	Format Format
	// FilePath, when non-empty, routes log entries through lumberjack to a
	// rotated file. Parent directory is created with mode 0700; the file is
	// touched with mode 0600 before lumberjack takes over.
	FilePath string
	// MaxSizeMB is the rotation threshold per file. Defaults to 10.
	MaxSizeMB int
	// MaxBackups is the number of historical files kept. Defaults to 5.
	MaxBackups int
}

// NewLogger returns a zap.Logger configured per cfg. When cfg.FilePath is set,
// writes go through a lumberjack-rotated file sink; otherwise to cfg.Output
// (defaulting to os.Stderr).
func NewLogger(cfg Config) (*zap.Logger, error) {
	encConf := zap.NewProductionEncoderConfig()
	encConf.TimeKey = "ts"
	encConf.EncodeTime = zapcore.ISO8601TimeEncoder
	encConf.MessageKey = "msg"
	encConf.LevelKey = "level"
	encConf.EncodeLevel = zapcore.LowercaseLevelEncoder

	var encoder zapcore.Encoder
	switch cfg.Format {
	case FormatText:
		encoder = zapcore.NewConsoleEncoder(encConf)
	default:
		encoder = zapcore.NewJSONEncoder(encConf)
	}

	var sink zapcore.WriteSyncer
	if cfg.FilePath == "" {
		out := cfg.Output
		if out == nil {
			out = os.Stderr
		}
		sink = zapcore.AddSync(out)
	} else {
		dir := filepath.Dir(cfg.FilePath)
		// MkdirAll creates missing components with mode 0700; existing
		// directories are left alone (unless they are Mazarbul-owned;
		// see below).
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create log dir %s: %w", dir, err)
		}
		// Tighten the parent's mode ONLY when it is the Mazarbul-owned
		// default cache dir. For user-supplied MAZ_LOG_FILE paths (e.g.
		// /tmp/maz.log, a relative path under cwd) the parent is shared
		// and must not be clobbered; for the default ~/.cache/mazarbul
		// the AC requires 0700 and we own the directory.
		if dir == config.OwnedDaemonLogDir() {
			// Lstat-gate the chmod: os.Chmod follows symlinks, so if
			// the owned path is a symlink to a shared directory we
			// would mutate the target. Skip the chmod in that case;
			// the file write still proceeds through the link.
			linkInfo, lerr := os.Lstat(dir)
			if lerr != nil {
				return nil, fmt.Errorf("lstat log dir %s: %w", dir, lerr)
			}
			if linkInfo.Mode()&os.ModeSymlink == 0 {
				// G302 flags any mode > 0600; 0700 is required for
				// owner traverse.
				if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: 0700 dir mode is required for owner traverse
					return nil, fmt.Errorf("chmod log dir %s: %w", dir, err)
				}
			}
		}
		f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("touch log file %s: %w", cfg.FilePath, err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("close log file %s: %w", cfg.FilePath, err)
		}
		// OpenFile's mode only applies when the file is created; tighten
		// an existing file's mode so a stale 0644 daemon.log left by an
		// earlier run does not stay world-readable. The path is the user's
		// designated log file, not a shared parent, so this is safe.
		if err := os.Chmod(cfg.FilePath, 0o600); err != nil {
			return nil, fmt.Errorf("chmod log file %s: %w", cfg.FilePath, err)
		}
		maxSize := cfg.MaxSizeMB
		if maxSize <= 0 {
			maxSize = defaultMaxSizeMB
		}
		maxBackups := cfg.MaxBackups
		if maxBackups <= 0 {
			maxBackups = defaultMaxBackups
		}
		sink = zapcore.AddSync(&lumberjack.Logger{
			Filename:   cfg.FilePath,
			MaxSize:    maxSize,
			MaxBackups: maxBackups,
			MaxAge:     0,
			Compress:   false,
			LocalTime:  false,
		})
	}

	core := zapcore.NewCore(encoder, sink, zapLevel(cfg.Level))
	return zap.New(core, zap.AddCaller()), nil
}

// MustNewLogger is the panic-on-error variant for callers that cannot fail
// (e.g. test setup). Production paths should call NewLogger and surface the
// error to main.
func MustNewLogger(cfg Config) *zap.Logger {
	l, err := NewLogger(cfg)
	if err != nil {
		panic(err)
	}
	return l
}

func zapLevel(l Level) zapcore.Level {
	switch l {
	case LevelDebug:
		return zapcore.DebugLevel
	case LevelInfo:
		return zapcore.InfoLevel
	case LevelWarn:
		return zapcore.WarnLevel
	case LevelError:
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}
