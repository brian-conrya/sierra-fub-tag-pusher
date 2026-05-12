package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/brian-conrya/sierra-fub-tag-pusher/internal/logging"
)

func TestNewWith_EmitsJSON(t *testing.T) {
	var buf bytes.Buffer
	log := logging.NewWith(&buf, slog.LevelInfo)
	log.Info("hello", slog.String("k", "v"))

	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &got); err != nil {
		t.Fatalf("not JSON: %v: %q", err, buf.String())
	}
	if got["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", got["msg"])
	}
	if got["k"] != "v" {
		t.Errorf("k = %v, want v", got["k"])
	}
}

func TestNew_ReturnsUsableLogger(t *testing.T) {
	log := logging.New(slog.LevelInfo)
	if log == nil {
		t.Fatal("New returned nil")
	}
	// Should not panic; writes to stderr in production.
	log.Info("smoke")
}

func TestNewWith_RespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := logging.NewWith(&buf, slog.LevelWarn)
	log.Info("filtered")
	if buf.Len() != 0 {
		t.Errorf("expected info to be filtered, got %q", buf.String())
	}
}
