package main

import (
	"log/slog"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"", slog.LevelInfo},
		{"info", slog.LevelInfo},
		{"INFO", slog.LevelInfo},
		{"  warn  ", slog.LevelWarn},
		{"WARN", slog.LevelWarn},
		{"error", slog.LevelError},
		{"debug", slog.LevelDebug},
		{"garbage", slog.LevelInfo},
	}
	for _, tc := range cases {
		if got := parseLogLevel(tc.in); got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestRequiredEnv(t *testing.T) {
	t.Setenv("SOMETHING", "value")
	v, err := requiredEnv("SOMETHING")
	if err != nil || v != "value" {
		t.Fatalf("got %q, %v; want \"value\", nil", v, err)
	}

	t.Setenv("EMPTY", "")
	if _, err := requiredEnv("EMPTY"); err == nil {
		t.Error("expected error for empty env")
	}

	t.Setenv("WHITESPACE", "   ")
	if _, err := requiredEnv("WHITESPACE"); err == nil {
		t.Error("expected error for whitespace-only env")
	}

	if _, err := requiredEnv("MAIN_TEST_DEFINITELY_UNSET"); err == nil {
		t.Error("expected error for unset env")
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("MAIN_TEST_PRESENT", "x")
	if got := envOr("MAIN_TEST_PRESENT", "fallback"); got != "x" {
		t.Errorf("envOr present = %q, want x", got)
	}
	if got := envOr("MAIN_TEST_DEFINITELY_UNSET", "fallback"); got != "fallback" {
		t.Errorf("envOr missing = %q, want fallback", got)
	}
	t.Setenv("MAIN_TEST_BLANK", "  ")
	if got := envOr("MAIN_TEST_BLANK", "fallback"); got != "fallback" {
		t.Errorf("envOr whitespace = %q, want fallback", got)
	}
}
