package connector

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
)

// SetupLogger configures the contract's JSON logging (lisa/docs/GO_CONNECTOR_SDK.md).
// It writes JSONL to LISA_MCP_LOG_PATH; when that is unset (server launched
// outside the host compiler) it falls back to <cwd>/<serverID>.jsonl and never
// to bare stderr. LISA_LOG_STDERR=1 adds an opt-in stderr mirror for debugging.
//
// The returned logger is safe for concurrent use. The caller owns closing the
// underlying file via the returned close func.
func SetupLogger(serverID, level string) (*slog.Logger, func() error, error) {
	lv := parseLevel(level)

	logPath := os.Getenv("LISA_MCP_LOG_PATH")
	if logPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "."
		}
		logPath = filepath.Join(cwd, serverID+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}

	var handler slog.Handler = slog.NewJSONHandler(f, &slog.HandlerOptions{Level: lv})
	if os.Getenv("LISA_LOG_STDERR") == "1" {
		handler = multiHandler{handler, slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lv})}
	}
	logger := slog.New(handler).With("server", serverID)
	return logger, f.Close, nil
}

func parseLevel(level string) slog.Level {
	switch level {
	case "DEBUG":
		return slog.LevelDebug
	case "WARNING":
		return slog.LevelWarn
	case "ERROR", "CRITICAL":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// multiHandler fans log records out to several handlers (primary file + stderr
// mirror when LISA_LOG_STDERR=1). A record is emitted if any handler is enabled.
type multiHandler []slog.Handler

func (m multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (m multiHandler) WithGroup(name string) slog.Handler {
	out := make(multiHandler, len(m))
	for i, h := range m {
		out[i] = h.WithGroup(name)
	}
	return out
}
