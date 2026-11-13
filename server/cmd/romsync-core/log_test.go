package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestConsoleLogFormat(t *testing.T) {
	for _, test := range []struct {
		level slog.Level
		color string
	}{
		{slog.LevelInfo, "\x1b[94m"},
		{slog.LevelWarn, "\x1b[33m"},
		{slog.LevelError, "\x1b[31m"},
	} {
		var output bytes.Buffer
		handler := newConsoleHandler(&output, true)
		record := slog.NewRecord(time.Date(2026, 1, 2, 12, 34, 56, 0, time.UTC), test.level, "dashboard listening", 0)
		record.AddAttrs(slog.String("url", "http://server.example:18777"))
		if err := handler.Handle(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		expected := "2026-01-02 12:34:56 " + test.color + "dashboard listening\x1b[0m url=http://server.example:18777\n"
		if output.String() != expected {
			t.Fatalf("unexpected console format: %q", output.String())
		}
	}
}

func TestConsoleLogContextAndPlainOutput(t *testing.T) {
	var output bytes.Buffer
	handler := newConsoleHandler(&output, false)
	logger := slog.New(handler).With("service", "web").WithGroup("request").With("id", 42)
	logger.Error("request failed", "error", errors.New("permission denied"), slog.Group("detail", "path", "bad\npath"))
	text := output.String()
	if strings.Contains(text, "\x1b") || !strings.Contains(text, `request failed service=web request.id=42 request.error=permission denied request.detail.path="bad\npath"`) {
		t.Fatalf("log context missing or unsafe: %q", text)
	}
	if strings.Count(text, "\n") != 1 {
		t.Fatalf("log entry spans multiple lines: %q", text)
	}
	output.Reset()
	slog.New(handler).Info("separate entry")
	if strings.Contains(output.String(), "service=") {
		t.Fatal("derived handler modified its parent")
	}
}
