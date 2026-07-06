package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestLoadUsage(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	setup := []string{
		`CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);`,
		`CREATE TABLE session_message (id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL, seq integer NOT NULL);`,
		fmt.Sprintf(`INSERT INTO message VALUES ('msg_1', 'ses_1', %d, %d, '{"role":"assistant","providerID":"anthropic","modelID":"claude-opus-4-5","tokens":{"input":10,"output":20,"reasoning":30,"cache":{"read":40,"write":50}}}');`, testMillis(2026, 1, 10), testMillis(2026, 1, 10)),
		fmt.Sprintf(`INSERT INTO message VALUES ('msg_2', 'ses_1', %d, %d, '{"role":"assistant","providerID":"anthropic","modelID":"claude-opus-4-5","tokens":{"input":1,"output":2,"reasoning":3,"cache":{"read":4,"write":5}}}');`, testMillis(2026, 1, 10), testMillis(2026, 1, 10)),
		fmt.Sprintf(`INSERT INTO message VALUES ('msg_3', 'ses_1', %d, %d, '{"role":"user"}');`, testMillis(2026, 1, 10), testMillis(2026, 1, 10)),
		fmt.Sprintf(`INSERT INTO session_message VALUES ('msg_4', 'ses_1', 'assistant', %d, %d, '{"model":{"providerID":"openai","id":"gpt-5.1"},"tokens":{"input":100,"output":200,"reasoning":300,"cache":{"read":400,"write":500}}}', 1);`, testMillis(2026, 1, 11), testMillis(2026, 1, 11)),
	}
	for _, query := range setup {
		if _, err := db.ExecContext(context.Background(), query); err != nil {
			t.Fatal(err)
		}
	}

	usage, err := loadUsage(context.Background(), db, testDate(2026, 1, 1))
	if err != nil {
		t.Fatal(err)
	}

	assertUsage(t, usage[dailyUsageKey{date: "2026-01-10", provider: "anthropic", model: "claude-opus-4-5"}], usageFromInts(11, 44, 55, 33, 22))
	assertUsage(t, usage[dailyUsageKey{date: "2026-01-11", provider: "openai", model: "gpt-5.1"}], usageFromInts(100, 400, 500, 300, 200))
}

func TestDecodePricing(t *testing.T) {
	prices, err := decodePricing(strings.NewReader(`{
		"anthropic": {
			"models": {
				"claude-opus-4-5": {"cost": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25}},
				"fractional": {"cost": {"input": 0.333333333333333333, "output": 0.1, "cache_read": 0.2, "cache_write": 0.3}},
				"free-model": {}
			}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	got := prices[usageKey{provider: "anthropic", model: "claude-opus-4-5"}]
	assertPricing(t, got, pricing{input: rat("5"), output: rat("25"), cacheRead: rat("0.5"), cacheWrite: rat("6.25")})
	assertPricing(t, prices[usageKey{provider: "anthropic", model: "fractional"}], pricing{
		input:      rat("0.333333333333333333"),
		output:     rat("0.1"),
		cacheRead:  rat("0.2"),
		cacheWrite: rat("0.3"),
	})
	if _, ok := prices[usageKey{provider: "anthropic", model: "free-model"}]; ok {
		t.Fatal("unexpected pricing for model without cost")
	}
}

func TestParseOpenCodeDataDir(t *testing.T) {
	got, err := parseOpenCodeDataDir(strings.Join([]string{
		"home       /Users/test",
		"data       /Users/test/Library/Application Support/opencode",
		"cache      /Users/test/.cache/opencode",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := "/Users/test/Library/Application Support/opencode"
	if got != want {
		t.Fatalf("data path mismatch: got %q, want %q", got, want)
	}
}

func TestParseOpenCodeDataDirMissingData(t *testing.T) {
	_, err := parseOpenCodeDataDir("home /Users/test\n")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestWriteCSV(t *testing.T) {
	var out bytes.Buffer
	err := writeCSV(&out, []row{{
		date:     "2026-01-01",
		provider: "TOTAL",
		model:    "TOTAL",
		usage:    usageFromInts(1_000_000, 2_000_000, 3_000_000, 4_000_000, 5_000_000),
		inCost:   rat("24.75"),
		outCost:  rat("225"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"date,provider,model,token in,cached in,total $ in,reasoning,out,total $ out,total $ i/o",
		"2026-01-01,TOTAL,TOTAL,1000000,2000000,24.750000,4000000,5000000,225.000000,249.750000",
		"",
	}, "\n")
	if out.String() != want {
		t.Fatalf("csv mismatch:\ngot:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestBuildRowsProjection(t *testing.T) {
	window := reportWindow{start: testDate(2026, 1, 1), today: testDate(2026, 1, 30)}
	modelKeyA := usageKey{provider: "anthropic", model: "claude-opus-4-5"}
	modelKeyB := usageKey{provider: "openai", model: "gpt-5.1"}
	rows := buildRows(map[dailyUsageKey]usageTotals{
		{date: "2026-01-01", provider: modelKeyA.provider, model: modelKeyA.model}: usageFromInts(30, 60, 90, 120, 150),
		{date: "2026-01-01", provider: modelKeyB.provider, model: modelKeyB.model}: usageFromInts(3, 6, 9, 12, 15),
		{date: "2026-01-30", provider: modelKeyA.provider, model: modelKeyA.model}: usageFromInts(60, 90, 120, 150, 180),
	}, map[usageKey]pricing{
		modelKeyA: {input: rat("1"), output: rat("2"), cacheRead: rat("3"), cacheWrite: rat("4")},
		modelKeyB: {input: rat("10"), output: rat("20"), cacheRead: rat("30"), cacheWrite: rat("40")},
	}, window)

	if len(rows) != 135 {
		t.Fatalf("row count mismatch: got %d, want 135", len(rows))
	}
	if rows[0].date != "2026-01-01" {
		t.Fatalf("first date mismatch: got %q", rows[0].date)
	}
	if rows[0].provider != "TOTAL" || rows[0].model != "TOTAL" {
		t.Fatalf("first row is not daily total: got %s/%s", rows[0].provider, rows[0].model)
	}
	assertUsage(t, rows[0].usage, usageFromInts(33, 66, 99, 132, 165))
	assertRat(t, "first inCost", rows[0].inCost, rat("57/50000"))
	assertRat(t, "first outCost", rows[0].outCost, rat("27/25000"))
	if rows[1].provider != "anthropic" || rows[1].model != "claude-opus-4-5" {
		t.Fatalf("first breakdown mismatch: got %s/%s", rows[1].provider, rows[1].model)
	}
	assertUsage(t, rows[1].usage, usageFromInts(30, 60, 90, 120, 150))
	if rows[3].date != "2026-01-02" {
		t.Fatalf("second date mismatch: got %q", rows[3].date)
	}
	assertUsage(t, rows[3].usage, usageTotals{})
	if rows[90].date != "2026-01-31" {
		t.Fatalf("first projection date mismatch: got %q", rows[90].date)
	}
	assertUsage(t, rows[90].usage, usageTotals{
		tokenIn:    rat("31/10"),
		cachedIn:   rat("26/5"),
		cacheWrite: rat("73/10"),
		reasoning:  rat("47/5"),
		out:        rat("23/2"),
	})
	assertRat(t, "projection inCost", rows[90].inCost, rat("13/200000"))
	assertRat(t, "projection outCost", rows[90].outCost, rat("29/500000"))
	if rows[132].date != "2026-02-14" {
		t.Fatalf("last projection date mismatch: got %q", rows[132].date)
	}
}

func TestWritePNG(t *testing.T) {
	var out bytes.Buffer
	err := writePNG(&out, []row{
		{
			date:     "2026-01-01",
			provider: "TOTAL",
			model:    "TOTAL",
			usage:    usageFromInts(1_000_000, 2_000_000, 3_000_000, 4_000_000, 5_000_000),
			inCost:   rat("24.75"),
			outCost:  rat("225"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pngBytes := out.Bytes()
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds().Dx(); got != 800 {
		t.Fatalf("width mismatch: got %d, want 800", got)
	}
	if got := img.Bounds().Dy(); got != 480 {
		t.Fatalf("height mismatch: got %d, want 480", got)
	}
	info, err := pngHeaderInfo(pngBytes)
	if err != nil {
		t.Fatal(err)
	}
	if info.bitDepth != 1 || info.colorType != 0 {
		t.Fatalf("PNG header bitDepth/colorType = %d/%d, want 1/0", info.bitDepth, info.colorType)
	}
}

func TestValidateTRMNLPNGRejectsContinuousGray(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, trmnlPNGWidth, trmnlPNGHeight))
	img.SetGray(0, 0, color.Gray{Y: 0x80})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	err := validateTRMNLPNG(out.Bytes())
	if err == nil || !strings.Contains(err.Error(), "1-bit 2-level grayscale") {
		t.Fatalf("expected 1-bit validation error, got %v", err)
	}
}

func TestValidateTRMNLPNGRejectsWrongSize(t *testing.T) {
	img := image.NewPaletted(image.Rect(0, 0, 10, 10), color.Palette{color.Gray{Y: 0xff}, color.Gray{Y: 0x00}})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	err := validateTRMNLPNG(out.Bytes())
	if err == nil || !strings.Contains(err.Error(), "got 10x10") {
		t.Fatalf("expected size validation error, got %v", err)
	}
}

func TestUploadTRMNLWebhookImagePostsPNG(t *testing.T) {
	pngBytes := validTRMNLPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "image/png" {
			t.Fatalf("content-type = %q, want image/png", got)
		}
		body := new(bytes.Buffer)
		if _, err := body.ReadFrom(r.Body); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body.Bytes(), pngBytes) {
			t.Fatalf("body mismatch")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := uploadTRMNLWebhookImage(context.Background(), server.Client(), server.URL, pngBytes); err != nil {
		t.Fatal(err)
	}
}

func TestUploadTRMNLWebhookImage422(t *testing.T) {
	pngBytes := validTRMNLPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad image", http.StatusUnprocessableEntity)
	}))
	defer server.Close()
	err := uploadTRMNLWebhookImage(context.Background(), server.Client(), server.URL, pngBytes)
	if err == nil || !strings.Contains(err.Error(), "over 5 MB, the wrong format, or corrupted") {
		t.Fatalf("expected 422 troubleshooting error, got %v", err)
	}
}

func TestUploadTRMNLWebhookImage429(t *testing.T) {
	pngBytes := validTRMNLPNG(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer server.Close()
	err := uploadTRMNLWebhookImage(context.Background(), server.Client(), server.URL, pngBytes)
	if err == nil || !strings.Contains(err.Error(), "12 uploads per hour") {
		t.Fatalf("expected 429 troubleshooting error, got %v", err)
	}
}

func TestNextDaemonRun(t *testing.T) {
	tests := []struct {
		now  time.Time
		want time.Time
	}{
		{testClock(10, 4, 59), testClock(10, 5, 0)},
		{testClock(10, 5, 0), testClock(10, 25, 0)},
		{testClock(10, 24, 0), testClock(10, 25, 0)},
		{testClock(10, 25, 0), testClock(10, 45, 0)},
		{testClock(10, 45, 0), testClock(11, 5, 0)},
	}
	for _, tt := range tests {
		if got := nextDaemonRun(tt.now); !got.Equal(tt.want) {
			t.Fatalf("nextDaemonRun(%s) = %s, want %s", tt.now, got, tt.want)
		}
	}
}

func assertUsage(t *testing.T, got usageTotals, want usageTotals) {
	t.Helper()
	assertRat(t, "tokenIn", nonNilRat(got.tokenIn), nonNilRat(want.tokenIn))
	assertRat(t, "cachedIn", nonNilRat(got.cachedIn), nonNilRat(want.cachedIn))
	assertRat(t, "cacheWrite", nonNilRat(got.cacheWrite), nonNilRat(want.cacheWrite))
	assertRat(t, "reasoning", nonNilRat(got.reasoning), nonNilRat(want.reasoning))
	assertRat(t, "out", nonNilRat(got.out), nonNilRat(want.out))
}

func assertPricing(t *testing.T, got pricing, want pricing) {
	t.Helper()
	assertRat(t, "input", got.input, want.input)
	assertRat(t, "output", got.output, want.output)
	assertRat(t, "cacheRead", got.cacheRead, want.cacheRead)
	assertRat(t, "cacheWrite", got.cacheWrite, want.cacheWrite)
}

func assertRat(t *testing.T, name string, got *big.Rat, want *big.Rat) {
	t.Helper()
	if got == nil || want == nil || got.Cmp(want) != 0 {
		t.Fatalf("%s mismatch: got %v, want %v", name, got, want)
	}
}

func validTRMNLPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, trmnlPNGWidth, trmnlPNGHeight))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	var out bytes.Buffer
	if err := encode1BitPNG(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func testClock(hour int, minute int, second int) time.Time {
	return time.Date(2026, 1, 2, hour, minute, second, 0, time.Local)
}

func rat(value string) *big.Rat {
	result, ok := new(big.Rat).SetString(value)
	if !ok {
		panic(value)
	}
	return result
}

func testDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.Local)
}

func testMillis(year int, month time.Month, day int) int64 {
	return testDate(year, month, day).UnixMilli()
}
