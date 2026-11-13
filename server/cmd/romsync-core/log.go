package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

type consoleHandler struct {
	output io.Writer
	mu     *sync.Mutex
	color  bool
	groups []string
	attrs  []string
}

func newConsoleHandler(output io.Writer, color bool) slog.Handler {
	return &consoleHandler{output: output, mu: &sync.Mutex{}, color: color}
}

func (h *consoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h *consoleHandler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	line.WriteString(record.Time.Format("2006-01-02 15:04:05"))
	line.WriteByte(' ')
	if h.color {
		switch {
		case record.Level >= slog.LevelError:
			line.WriteString("\x1b[31m")
		case record.Level >= slog.LevelWarn:
			line.WriteString("\x1b[33m")
		default:
			line.WriteString("\x1b[94m")
		}
	}
	line.WriteString(logText(record.Message))
	if h.color {
		line.WriteString("\x1b[0m")
	}
	for _, attr := range h.attrs {
		line.WriteByte(' ')
		line.WriteString(attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		for _, text := range consoleAttrs(h.groups, attr) {
			line.WriteByte(' ')
			line.WriteString(text)
		}
		return true
	})
	line.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.output, line.String())
	return err
}

func (h *consoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append([]string(nil), h.attrs...)
	for _, attr := range attrs {
		next.attrs = append(next.attrs, consoleAttrs(h.groups, attr)...)
	}
	return &next
}

func (h *consoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string(nil), h.groups...), name)
	return &next
}

func consoleAttrs(groups []string, attr slog.Attr) []string {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return nil
	}
	if attr.Value.Kind() == slog.KindGroup {
		if attr.Key != "" {
			groups = append(append([]string(nil), groups...), attr.Key)
		}
		var result []string
		for _, child := range attr.Value.Group() {
			result = append(result, consoleAttrs(groups, child)...)
		}
		return result
	}
	key := strings.Join(append(append([]string(nil), groups...), attr.Key), ".")
	return []string{logText(key) + "=" + logText(fmt.Sprint(attr.Value.Any()))}
}

func logText(text string) string {
	if strings.ContainsFunc(text, unicode.IsControl) {
		return strconv.Quote(text)
	}
	return text
}
