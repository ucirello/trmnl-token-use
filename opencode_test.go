package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoadV2Usage(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 3, 7, 0, 0, 0, 0, loc)
	window := reportWindow{start: start, today: start.AddDate(0, 0, 29)}
	calls := 0
	usage, err := loadV2Usage(context.Background(), window, func(ctx context.Context, path string) ([]byte, error) {
		u, err := url.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		day := start.AddDate(0, 0, calls)
		if u.Path != "/api/experimental/session/stats" || q.Get("tools") != "none" || q.Has("project") {
			t.Fatalf("unexpected request: %s", path)
		}
		if q.Get("from") != strconv.FormatInt(day.UnixMilli(), 10) || q.Get("to") != strconv.FormatInt(day.AddDate(0, 0, 1).UnixMilli(), 10) {
			t.Fatalf("wrong local-day boundaries: %s", path)
		}
		calls++
		models := `[]`
		if calls == 2 {
			models = `[
				{"model":{"providerID":"openai","id":"test","variant":"high"},"tokens":{"input":10,"output":20,"reasoning":30,"cache":{"read":40,"write":50}}},
				{"model":{"providerID":"openai","id":"test","variant":"low"},"tokens":{"input":1,"output":2,"reasoning":3,"cache":{"read":4,"write":5}}}
			]`
		}
		return []byte(fmt.Sprintf(`{"data":{"range":{"from":%s,"to":%s},"models":%s}}`, q.Get("from"), q.Get("to"), models)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 30 || len(usage) != 1 {
		t.Fatalf("calls=%d groups=%d, want 30 and 1", calls, len(usage))
	}
	assertUsage(t, usage[dailyUsageKey{date: "2026-03-08", provider: "openai", model: "test"}], usageFromInts(11, 44, 55, 33, 22))
}

func TestLoadV2UsageRejectsIncompleteReports(t *testing.T) {
	window := newReportWindow(time.Now())
	for _, body := range []string{`invalid`, `{}`, `{"data":{}}`, `{"data":{"range":{"from":0,"to":1},"models":[]}}`} {
		t.Run(body, func(t *testing.T) {
			usage, err := loadV2Usage(context.Background(), window, func(context.Context, string) ([]byte, error) {
				return []byte(body), nil
			})
			if err == nil || usage != nil {
				t.Fatalf("got usage=%v err=%v, want failure without partial results", usage, err)
			}
		})
	}
	want := errors.New("service unavailable")
	_, err := loadV2Usage(context.Background(), window, func(context.Context, string) ([]byte, error) { return nil, want })
	if !errors.Is(err, want) || !strings.Contains(err.Error(), window.start.Format(time.DateOnly)) {
		t.Fatalf("missing underlying error or date: %v", err)
	}
}

func TestLoadV2UsageRejectsMissingTokens(t *testing.T) {
	window := newReportWindow(time.Now())
	_, err := loadV2Usage(context.Background(), window, func(_ context.Context, path string) ([]byte, error) {
		u, _ := url.Parse(path)
		q := u.Query()
		return []byte(fmt.Sprintf(`{"data":{"range":{"from":%s,"to":%s},"models":[{"model":{"providerID":"openai","id":"test"}}]}}`, q.Get("from"), q.Get("to"))), nil
	})
	if err == nil {
		t.Fatal("missing usage must not silently become zero")
	}
}

func TestQueryOpenCodeAPI(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode2")
	// Exercise the actual subprocess arguments, stdout, stderr, and exit status.
	script := `#!/bin/sh
if [ "$1" != api ] || [ "$2" != get ]; then exit 2; fi
if [ "$3" = /failure ]; then echo 'HTTP 503 Service Unavailable' >&2; exit 1; fi
printf '{"path":"%s"}' "$3"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	body, err := queryOpenCodeAPI(context.Background(), "/api/experimental/session/stats?from=1&to=2&tools=none")
	if err != nil || string(body) != `{"path":"/api/experimental/session/stats?from=1&to=2&tools=none"}` {
		t.Fatalf("unexpected command output: %s, %v", body, err)
	}
	_, err = queryOpenCodeAPI(context.Background(), "/failure")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("missing command failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := queryOpenCodeAPI(ctx, "/cancelled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
}

func TestExplicitDatabaseDoesNotCreateMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	if _, err := loadReportUsage(context.Background(), path, newReportWindow(time.Now())); err == nil {
		t.Fatal("missing database should fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("database was created: %v", err)
	}
}
