package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"

	"openai-proxy/internal/config"
)

const defaultMaxSizeMB = 100

func New(cfg config.LoggingConfig) (*slog.Logger, io.Closer, error) {
	if err := os.MkdirAll(cfg.Path, 0o755); err != nil {
		return nil, nil, err
	}

	file := &lumberjack.Logger{
		Filename:   filepath.Join(cfg.Path, "openai-proxy.log"),
		MaxSize:    defaultMaxSizeMB,
		MaxBackups: cfg.MaxFiles - 1,
		Compress:   true,
	}
	writer := io.MultiWriter(os.Stderr, file)
	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, file, nil
}
