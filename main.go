package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/opts"

	_ "modernc.org/sqlite"
)

const modelsDevURL = "https://models.dev/api.json"

const (
	historyDays    = 30
	projectionDays = 15
	trmnlPNGWidth  = 800
	trmnlPNGHeight = 480
	trmnlPNGMax    = 5 * 1024 * 1024
)

const trmnlWebhookEnv = "TRMNL_TOKEN_USAGE_WEBHOOK"

type usageKey struct {
	provider string
	model    string
}

type dailyUsageKey struct {
	date     string
	provider string
	model    string
}

type usageTotals struct {
	tokenIn    *big.Rat
	cachedIn   *big.Rat
	cacheWrite *big.Rat
	reasoning  *big.Rat
	out        *big.Rat
}

type pricing struct {
	input      *big.Rat
	output     *big.Rat
	cacheRead  *big.Rat
	cacheWrite *big.Rat
}

type row struct {
	date     string
	provider string
	model    string
	usage    usageTotals
	inCost   *big.Rat
	outCost  *big.Rat
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		log.Fatalln(err)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := io.WriteString(stdout, helpText())
		return err
	}
	command := "daemon"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "daemon":
		cfg, err := parseSharedFlags(command, args)
		if err != nil {
			return err
		}
		return runDaemon(ctx, func(ctx context.Context) error {
			return pushReport(ctx, cfg, http.DefaultClient, stdout)
		}, sleepContext, time.Now)
	case "ship":
		cfg, err := parseSharedFlags(command, args)
		if err != nil {
			return err
		}
		return pushReport(ctx, cfg, http.DefaultClient, stdout)
	case "render":
		cfg, err := parseRenderFlags(args)
		if err != nil {
			return err
		}
		return renderReport(ctx, cfg, stdout)
	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, helpText())
	}
}

type cliConfig struct {
	dbPath    string
	modelsURL string
	format    string
	outPath   string
}

func parseSharedFlags(name string, args []string) (cliConfig, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dbPath := flags.String("db", "", "path to opencode sqlite database")
	modelsURL := flags.String("models", modelsDevURL, "models.dev api json url")
	if err := flags.Parse(args); err != nil {
		return cliConfig{}, err
	}
	return cliConfig{dbPath: *dbPath, modelsURL: *modelsURL}, nil
}

func parseRenderFlags(args []string) (cliConfig, error) {
	flags := flag.NewFlagSet("render", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dbPath := flags.String("db", "", "path to opencode sqlite database")
	modelsURL := flags.String("models", modelsDevURL, "models.dev api json url")
	format := flags.String("format", "png", "output format: csv or png")
	outPath := flags.String("out", "token-usage.png", "output path for png; csv always writes to stdout")
	if err := flags.Parse(args); err != nil {
		return cliConfig{}, err
	}
	return cliConfig{dbPath: *dbPath, modelsURL: *modelsURL, format: *format, outPath: *outPath}, nil
}

func renderReportRows(ctx context.Context, cfg cliConfig) ([]row, error) {
	dbPath := cfg.dbPath
	if dbPath == "" {
		path, err := defaultDBPath(ctx)
		if err != nil {
			return nil, err
		}
		dbPath = path
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open opencode database: %w", err)
	}
	defer db.Close()
	window := newReportWindow(time.Now())
	usage, err := loadUsage(ctx, db, window.start)
	if err != nil {
		return nil, err
	}
	prices, err := fetchPricing(ctx, cfg.modelsURL)
	if err != nil {
		return nil, err
	}
	return buildRows(usage, prices, window), nil
}

func renderReport(ctx context.Context, cfg cliConfig, stdout io.Writer) error {
	rows, err := renderReportRows(ctx, cfg)
	if err != nil {
		return err
	}
	switch cfg.format {
	case "csv":
		return writeCSV(stdout, rows)
	case "png":
		outPath := cfg.outPath
		if outPath == "" {
			outPath = "token-usage.png"
		}
		return writePNGs(outPath, rows)
	default:
		return fmt.Errorf("unknown render format %q", cfg.format)
	}
}

func renderPNGBytes(ctx context.Context, cfg cliConfig) ([]byte, error) {
	rows, err := renderReportRows(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writePNG(&out, rows); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func pushReport(ctx context.Context, cfg cliConfig, client *http.Client, stdout io.Writer) error {
	webhookURL := os.Getenv(trmnlWebhookEnv)
	if webhookURL == "" {
		return fmt.Errorf("%s is required for TRMNL upload", trmnlWebhookEnv)
	}
	pngBytes, err := renderPNGBytes(ctx, cfg)
	if err != nil {
		return err
	}
	if err := uploadTRMNLWebhookImage(ctx, client, webhookURL, pngBytes); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "TRMNL upload accepted: 800x480 2-level grayscale PNG, %d bytes\n", len(pngBytes))
	return err
}

func validateTRMNLPNG(pngBytes []byte) error {
	if len(pngBytes) > trmnlPNGMax {
		return fmt.Errorf("generated image is too large for TRMNL Webhook Image: %d bytes, max %d", len(pngBytes), trmnlPNGMax)
	}
	info, err := pngHeaderInfo(pngBytes)
	if err != nil {
		return err
	}
	if info.width != trmnlPNGWidth || info.height != trmnlPNGHeight {
		return fmt.Errorf("generated image is not TRMNL-ready: got %dx%d, want %dx%d", info.width, info.height, trmnlPNGWidth, trmnlPNGHeight)
	}
	if info.bitDepth != 1 || info.colorType != 0 {
		return fmt.Errorf("generated image is not TRMNL-ready: expected 1-bit 2-level grayscale PNG")
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return fmt.Errorf("generated image is not a valid PNG: %w", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() != trmnlPNGWidth || bounds.Dy() != trmnlPNGHeight {
		return fmt.Errorf("generated image is not TRMNL-ready: got %dx%d, want %dx%d", bounds.Dx(), bounds.Dy(), trmnlPNGWidth, trmnlPNGHeight)
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y != 0x00 && color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y != 0xff {
				return fmt.Errorf("generated image is not TRMNL-ready: expected 1-bit 2-level grayscale PNG")
			}
		}
	}
	return nil
}

type pngInfo struct {
	width     int
	height    int
	bitDepth  byte
	colorType byte
}

func pngHeaderInfo(pngBytes []byte) (pngInfo, error) {
	if len(pngBytes) < 33 || !bytes.Equal(pngBytes[:8], []byte{137, 80, 78, 71, 13, 10, 26, 10}) {
		return pngInfo{}, fmt.Errorf("generated image is not a valid PNG")
	}
	if string(pngBytes[12:16]) != "IHDR" {
		return pngInfo{}, fmt.Errorf("generated image is not a valid PNG: missing IHDR")
	}
	return pngInfo{
		width:     int(binary.BigEndian.Uint32(pngBytes[16:20])),
		height:    int(binary.BigEndian.Uint32(pngBytes[20:24])),
		bitDepth:  pngBytes[24],
		colorType: pngBytes[25],
	}, nil
}

func uploadTRMNLWebhookImage(ctx context.Context, client *http.Client, webhookURL string, pngBytes []byte) error {
	if err := validateTRMNLPNG(pngBytes); err != nil {
		return err
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(pngBytes))
	if err != nil {
		return fmt.Errorf("create TRMNL webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "image/png")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("TRMNL upload failed before receiving a response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	bodyText := strings.TrimSpace(string(body))
	switch resp.StatusCode {
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("TRMNL rejected image: HTTP 422. The image may be over 5 MB, the wrong format, or corrupted. Generated image: image/png, %dx%d, 1-bit 2-level grayscale, %d bytes. Response: %s", trmnlPNGWidth, trmnlPNGHeight, len(pngBytes), bodyText)
	case http.StatusTooManyRequests:
		return fmt.Errorf("TRMNL rate limit hit: HTTP 429. Webhook Image allows 12 uploads per hour. Wait before sending more images. Response: %s", bodyText)
	default:
		return fmt.Errorf("TRMNL upload failed: HTTP %d. Response: %s", resp.StatusCode, bodyText)
	}
}

func nextDaemonRun(now time.Time) time.Time {
	base := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, now.Location())
	for _, minute := range []int{5, 25, 45} {
		candidate := base.Add(time.Duration(minute) * time.Minute)
		if candidate.After(now) {
			return candidate
		}
	}
	return base.Add(time.Hour + 5*time.Minute)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runDaemon(ctx context.Context, runOnce func(context.Context) error, sleep func(context.Context, time.Duration) error, now func() time.Time) error {
	for {
		current := now()
		next := nextDaemonRun(current)
		if err := sleep(ctx, next.Sub(current)); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if err := runOnce(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "%s TRMNL upload failed: %v\n", time.Now().Format(time.RFC3339), err)
			continue
		}
		fmt.Fprintf(os.Stderr, "%s TRMNL upload accepted\n", time.Now().Format(time.RFC3339))
	}
}

func helpText() string {
	return `Usage:
  trmnl-token-use [ship|render] [flags]

Default:
  trmnl-token-use
        Run as a daemon. Generate and push a PNG to TRMNL at :05, :25, and :45.

Commands:
  ship
        Generate and push one PNG to TRMNL immediately, then exit.

  render
        Generate output locally. Supports PNG or CSV.

Flags:
  -db string
        path to opencode sqlite database
        default: discovered with "opencode debug paths"

  -models string
        models.dev API JSON URL
        default: https://models.dev/api.json

  -format string
        output format for render: png or csv
        default: png

  -out string
        output path for render -format png
        default: token-usage.png

Environment:
  TRMNL_TOKEN_USAGE_WEBHOOK
        required for default daemon mode and ship.
        Must be the TRMNL Webhook Image URL.
`
}

type reportWindow struct {
	start time.Time
	today time.Time
}

func newReportWindow(now time.Time) reportWindow {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	return reportWindow{
		start: today.AddDate(0, 0, -(historyDays - 1)),
		today: today,
	}
}

func defaultDBPath(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "opencode", "debug", "paths")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run opencode debug paths: %w", err)
	}
	dataDir, err := parseOpenCodeDataDir(string(output))
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "opencode.db"), nil
}

func parseOpenCodeDataDir(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "data" {
			return strings.Join(fields[1:], " "), nil
		}
	}
	return "", fmt.Errorf("parse opencode debug paths: missing data path")
}

func loadUsage(ctx context.Context, db *sql.DB, start time.Time) (map[dailyUsageKey]usageTotals, error) {
	result := make(map[dailyUsageKey]usageTotals)
	if err := loadLegacyMessageUsage(ctx, db, start, result); err != nil {
		return nil, err
	}
	if exists, err := tableExists(ctx, db, "session_message"); err != nil {
		return nil, err
	} else if exists {
		if err := loadSessionMessageUsage(ctx, db, start, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func loadLegacyMessageUsage(ctx context.Context, db *sql.DB, start time.Time, result map[dailyUsageKey]usageTotals) error {
	const query = `
		SELECT
			strftime('%Y-%m-%d', time_created / 1000, 'unixepoch', 'localtime'),
			json_extract(data, '$.providerID'),
			json_extract(data, '$.modelID'),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.input'), 0)), 0),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.cache.read'), 0)), 0),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.cache.write'), 0)), 0),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.reasoning'), 0)), 0),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.output'), 0)), 0)
		FROM
			message
		WHERE
			json_extract(data, '$.role') = 'assistant'
			AND time_created >= ?
		GROUP BY
			1, 2, 3
	`
	return scanUsage(ctx, db, query, start, result)
}

func loadSessionMessageUsage(ctx context.Context, db *sql.DB, start time.Time, result map[dailyUsageKey]usageTotals) error {
	const query = `
		SELECT
			strftime('%Y-%m-%d', time_created / 1000, 'unixepoch', 'localtime'),
			json_extract(data, '$.model.providerID'),
			json_extract(data, '$.model.id'),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.input'), 0)), 0),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.cache.read'), 0)), 0),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.cache.write'), 0)), 0),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.reasoning'), 0)), 0),
			coalesce(sum(coalesce(json_extract(data, '$.tokens.output'), 0)), 0)
		FROM
			session_message
		WHERE
			type = 'assistant'
			AND time_created >= ?
		GROUP BY
			1, 2, 3
	`
	return scanUsage(ctx, db, query, start, result)
}

func scanUsage(ctx context.Context, db *sql.DB, query string, start time.Time, result map[dailyUsageKey]usageTotals) error {
	rows, err := db.QueryContext(ctx, query, start.UnixMilli())
	if err != nil {
		return fmt.Errorf("query usage: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var date, provider, model sql.NullString
		var tokenIn, cachedIn, cacheWrite, reasoning, out int64
		if err := rows.Scan(&date, &provider, &model, &tokenIn, &cachedIn, &cacheWrite, &reasoning, &out); err != nil {
			return fmt.Errorf("scan usage: %w", err)
		}
		if !date.Valid || !provider.Valid || !model.Valid || date.String == "" || provider.String == "" || model.String == "" {
			continue
		}
		key := dailyUsageKey{date: date.String, provider: provider.String, model: model.String}
		current := result[key]
		current = addUsage(current, usageFromInts(tokenIn, cachedIn, cacheWrite, reasoning, out))
		result[key] = current
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read usage: %w", err)
	}
	return nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check table %s: %w", name, err)
	}
	return count > 0, nil
}

func fetchPricing(ctx context.Context, url string) (map[usageKey]pricing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch models: unexpected status %s", resp.Status)
	}

	return decodePricing(resp.Body)
}

func decodePricing(r io.Reader) (map[usageKey]pricing, error) {
	var raw map[string]struct {
		Models map[string]struct {
			Cost *struct {
				Input      json.Number `json:"input"`
				Output     json.Number `json:"output"`
				CacheRead  json.Number `json:"cache_read"`
				CacheWrite json.Number `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	decoder := json.NewDecoder(r)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}

	result := make(map[usageKey]pricing)
	for provider, providerData := range raw {
		for model, modelData := range providerData.Models {
			if modelData.Cost == nil {
				continue
			}
			price, err := parsePricing(modelData.Cost.Input, modelData.Cost.Output, modelData.Cost.CacheRead, modelData.Cost.CacheWrite)
			if err != nil {
				return nil, fmt.Errorf("parse pricing for %s/%s: %w", provider, model, err)
			}
			result[usageKey{provider: provider, model: model}] = price
		}
	}
	return result, nil
}

func parsePricing(input, output, cacheRead, cacheWrite json.Number) (pricing, error) {
	inputRat, err := parseRate(input)
	if err != nil {
		return pricing{}, fmt.Errorf("input: %w", err)
	}
	outputRat, err := parseRate(output)
	if err != nil {
		return pricing{}, fmt.Errorf("output: %w", err)
	}
	cacheReadRat, err := parseRate(cacheRead)
	if err != nil {
		return pricing{}, fmt.Errorf("cache_read: %w", err)
	}
	cacheWriteRat, err := parseRate(cacheWrite)
	if err != nil {
		return pricing{}, fmt.Errorf("cache_write: %w", err)
	}
	return pricing{
		input:      inputRat,
		output:     outputRat,
		cacheRead:  cacheReadRat,
		cacheWrite: cacheWriteRat,
	}, nil
}

func parseRate(value json.Number) (*big.Rat, error) {
	if value == "" {
		return new(big.Rat), nil
	}
	rat, ok := new(big.Rat).SetString(value.String())
	if !ok {
		return nil, fmt.Errorf("invalid rate %q", value.String())
	}
	return rat, nil
}

func buildRows(usage map[dailyUsageKey]usageTotals, prices map[usageKey]pricing, window reportWindow) []row {
	totalsByModel := make(map[usageKey]usageTotals)
	models := make(map[usageKey]struct{})
	for key, totals := range usage {
		modelKey := usageKey{provider: key.provider, model: key.model}
		models[modelKey] = struct{}{}
		totalsByModel[modelKey] = addUsage(totalsByModel[modelKey], totals)
	}

	modelKeys := make([]usageKey, 0, len(models))
	for key := range models {
		modelKeys = append(modelKeys, key)
	}
	sort.Slice(modelKeys, func(i, j int) bool {
		if modelKeys[i].provider != modelKeys[j].provider {
			return modelKeys[i].provider < modelKeys[j].provider
		}
		return modelKeys[i].model < modelKeys[j].model
	})

	rows := make([]row, 0, len(modelKeys)*(historyDays+projectionDays)+(historyDays+projectionDays))
	totalsByDate := make(map[string]row, historyDays+projectionDays)
	for day := 0; day < historyDays; day++ {
		date := window.start.AddDate(0, 0, day).Format(time.DateOnly)
		for _, modelKey := range modelKeys {
			item := buildModelRow(date, modelKey, usage[dailyUsageKey{
				date:     date,
				provider: modelKey.provider,
				model:    modelKey.model,
			}], prices[modelKey])
			rows = append(rows, item)
			addDailyTotal(totalsByDate, item)
		}
	}
	for day := 1; day <= projectionDays; day++ {
		date := window.today.AddDate(0, 0, day).Format(time.DateOnly)
		for _, modelKey := range modelKeys {
			item := buildModelRow(date, modelKey, averageUsage(totalsByModel[modelKey], historyDays), prices[modelKey])
			rows = append(rows, item)
			addDailyTotal(totalsByDate, item)
		}
	}

	for _, item := range totalsByDate {
		rows = append(rows, item)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].date != rows[j].date {
			return rows[i].date < rows[j].date
		}
		if rows[i].provider != rows[j].provider {
			return rows[i].provider < rows[j].provider
		}
		return rows[i].model < rows[j].model
	})
	return rows
}

func buildModelRow(date string, key usageKey, usage usageTotals, price pricing) row {
	return row{
		date:     date,
		provider: key.provider,
		model:    key.model,
		usage:    usage,
		inCost:   inputCost(usage, price),
		outCost:  outputCost(usage, price),
	}
}

func addDailyTotal(rows map[string]row, item row) {
	total := rows[item.date]
	total.date = item.date
	total.provider = "TOTAL"
	total.model = "TOTAL"
	total.usage = addUsage(total.usage, item.usage)
	total.inCost = new(big.Rat).Add(nonNilRat(total.inCost), nonNilRat(item.inCost))
	total.outCost = new(big.Rat).Add(nonNilRat(total.outCost), nonNilRat(item.outCost))
	rows[item.date] = total
}

func writeCSV(w io.Writer, rows []row) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{
		"date",
		"provider",
		"model",
		"token in",
		"cached in",
		"total $ in",
		"reasoning",
		"out",
		"total $ out",
		"total $ i/o",
	}); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	for _, item := range rows {
		inCost := nonNilRat(item.inCost)
		outCost := nonNilRat(item.outCost)
		totalCost := new(big.Rat).Add(inCost, outCost)
		if err := cw.Write([]string{
			item.date,
			item.provider,
			item.model,
			formatToken(item.usage.tokenIn),
			formatToken(item.usage.cachedIn),
			formatCost(inCost),
			formatToken(item.usage.reasoning),
			formatToken(item.usage.out),
			formatCost(outCost),
			formatCost(totalCost),
		}); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush csv: %w", err)
	}
	return nil
}

func inputCost(usage usageTotals, price pricing) *big.Rat {
	sum := new(big.Rat)
	sum.Add(sum, tokenCost(usage.tokenIn, price.input))
	sum.Add(sum, tokenCost(usage.cachedIn, price.cacheRead))
	sum.Add(sum, tokenCost(usage.cacheWrite, price.cacheWrite))
	return sum
}

func outputCost(usage usageTotals, price pricing) *big.Rat {
	return tokenCost(new(big.Rat).Add(nonNilRat(usage.out), nonNilRat(usage.reasoning)), price.output)
}

func tokenCost(tokens *big.Rat, rate *big.Rat) *big.Rat {
	if rate == nil || tokens == nil || tokens.Sign() == 0 {
		return new(big.Rat)
	}
	cost := new(big.Rat).Mul(tokens, rate)
	return cost.Quo(cost, big.NewRat(1_000_000, 1))
}

func usageFromInts(tokenIn, cachedIn, cacheWrite, reasoning, out int64) usageTotals {
	return usageTotals{
		tokenIn:    new(big.Rat).SetInt64(tokenIn),
		cachedIn:   new(big.Rat).SetInt64(cachedIn),
		cacheWrite: new(big.Rat).SetInt64(cacheWrite),
		reasoning:  new(big.Rat).SetInt64(reasoning),
		out:        new(big.Rat).SetInt64(out),
	}
}

func addUsage(a, b usageTotals) usageTotals {
	return usageTotals{
		tokenIn:    new(big.Rat).Add(nonNilRat(a.tokenIn), nonNilRat(b.tokenIn)),
		cachedIn:   new(big.Rat).Add(nonNilRat(a.cachedIn), nonNilRat(b.cachedIn)),
		cacheWrite: new(big.Rat).Add(nonNilRat(a.cacheWrite), nonNilRat(b.cacheWrite)),
		reasoning:  new(big.Rat).Add(nonNilRat(a.reasoning), nonNilRat(b.reasoning)),
		out:        new(big.Rat).Add(nonNilRat(a.out), nonNilRat(b.out)),
	}
}

func averageUsage(usage usageTotals, days int64) usageTotals {
	divisor := big.NewRat(days, 1)
	return usageTotals{
		tokenIn:    new(big.Rat).Quo(nonNilRat(usage.tokenIn), divisor),
		cachedIn:   new(big.Rat).Quo(nonNilRat(usage.cachedIn), divisor),
		cacheWrite: new(big.Rat).Quo(nonNilRat(usage.cacheWrite), divisor),
		reasoning:  new(big.Rat).Quo(nonNilRat(usage.reasoning), divisor),
		out:        new(big.Rat).Quo(nonNilRat(usage.out), divisor),
	}
}

func nonNilRat(value *big.Rat) *big.Rat {
	if value == nil {
		return new(big.Rat)
	}
	return value
}

func formatToken(value *big.Rat) string {
	if value == nil {
		return "0"
	}
	if value.IsInt() {
		return value.Num().String()
	}
	return value.FloatString(6)
}

func formatCost(value *big.Rat) string {
	if value == nil {
		return "0.000000"
	}
	return value.FloatString(6)
}

type graphMetric struct {
	suffix      string
	title       string
	yAxis       string
	prefix      string
	plotDivisor *big.Rat
	value       func(row) *big.Rat
}

func graphMetrics() []graphMetric {
	return []graphMetric{
		{
			suffix:      "token-total",
			title:       "opencode cumulative token total",
			yAxis:       "M tokens total",
			plotDivisor: big.NewRat(1_000_000, 1),
			value: func(item row) *big.Rat {
				input := tokenInput(item.usage)
				output := tokenOutput(item.usage)
				return input.Add(input, output)
			},
		},
	}
}

func writePNGs(outPath string, rows []row) error {
	metrics := graphMetrics()
	for _, metric := range metrics {
		path := outPath
		if len(metrics) > 1 {
			path = metricOutputPath(outPath, metric.suffix)
		}
		file, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("create %s png: %w", metric.suffix, err)
		}
		err = writeMetricPNG(file, rows, metric)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return fmt.Errorf("close %s png: %w", metric.suffix, closeErr)
		}
	}
	return nil
}

func metricOutputPath(outPath string, suffix string) string {
	ext := filepath.Ext(outPath)
	if ext == "" {
		ext = ".png"
	}
	stem := strings.TrimSuffix(outPath, filepath.Ext(outPath))
	return stem + "-" + suffix + ext
}

func writePNG(w io.Writer, rows []row) error {
	return writeMetricPNG(w, rows, graphMetrics()[0])
}

func writeMetricPNG(w io.Writer, rows []row, metric graphMetric) error {
	daily := totalRows(rows)
	modelKeys := modelKeysForRows(rows)
	line := charts.NewLine()
	line.SetGlobalOptions(
		charts.WithInitializationOpts(opts.Initialization{
			ChartID:  "token_usage_chart",
			Width:    "800px",
			Height:   "480px",
			Renderer: "svg",
		}),
		charts.WithTitleOpts(opts.Title{
			Title: metric.title,
			Left:  "24px",
			Top:   "12px",
		}),
		charts.WithLegendOpts(opts.Legend{Show: opts.Bool(false)}),
		charts.WithColorsOpts(opts.Colors{"#111111", "#444444", "#666666", "#888888", "#aaaaaa", "#c0c0c0"}),
		charts.WithGridOpts(opts.Grid{Left: "82px", Right: "24px", Top: "96px", Bottom: "64px"}),
		charts.WithXAxisOpts(opts.XAxis{AxisLabel: &opts.AxisLabel{Rotate: 45}}),
		charts.WithYAxisOpts(opts.YAxis{SplitLine: &opts.SplitLine{Show: opts.Bool(true)}}),
		charts.WithTooltipOpts(opts.Tooltip{Show: opts.Bool(true), Trigger: "axis"}),
	)

	dates := make([]string, 0, len(daily))
	for _, item := range daily {
		dates = append(dates, item.date[5:])
	}

	line.SetXAxis(dates)
	totalOpts := lineSeriesOpts(0)
	if marker, ok := todayMarker(daily, metric); ok {
		totalOpts = append(totalOpts, charts.WithMarkPointNameCoordItemOpts(marker))
	}
	line.AddSeries("TOTAL", cumulativeSeries(daily, metric), totalOpts...)
	for index, key := range modelKeys {
		seriesRows := modelRows(rows, key)
		seriesOpts := lineSeriesOpts(index + 1)
		if marker, ok := todayMarker(seriesRows, metric); ok {
			seriesOpts = append(seriesOpts, charts.WithMarkPointNameCoordItemOpts(marker))
		}
		line.AddSeries(key.provider+"/"+key.model, cumulativeSeries(seriesRows, metric), seriesOpts...)
	}

	var html bytes.Buffer
	if err := line.Render(&html); err != nil {
		return fmt.Errorf("render echarts html: %w", err)
	}
	localized, cleanup, err := localizeECharts(html.Bytes())
	if err != nil {
		return err
	}
	defer cleanup()
	localized, err = injectChartGraphics(localized, metric.yAxis, todayGraphicLabels(rows, metric), endGraphicLabels(rows, metric))
	if err != nil {
		return err
	}
	return screenshotChartHTML(w, localized)
}

func lineSeriesOpts(index int) []charts.SeriesOpts {
	styles := []struct {
		lineType string
		width    float32
	}{
		{lineType: "solid", width: 3},
		{lineType: "dashed", width: 2},
		{lineType: "dotted", width: 2},
		{lineType: "solid", width: 1.5},
		{lineType: "dashed", width: 3},
		{lineType: "dotted", width: 3},
	}
	style := styles[index%len(styles)]
	return []charts.SeriesOpts{
		charts.WithLineChartOpts(opts.LineChart{
			Symbol:       "none",
			SymbolSize:   0,
			ShowSymbol:   opts.Bool(false),
			Smooth:       opts.Bool(true),
			ConnectNulls: opts.Bool(true),
		}),
		charts.WithLineStyleOpts(opts.LineStyle{
			Type:  style.lineType,
			Width: style.width,
		}),
	}
}

func todayMarker(rows []row, metric graphMetric) (opts.MarkPointNameCoordItem, bool) {
	if len(rows) < historyDays {
		return opts.MarkPointNameCoordItem{}, false
	}
	running := new(big.Rat)
	for i := 0; i < historyDays; i++ {
		running.Add(running, nonNilRat(metric.value(rows[i])))
	}
	plotted := new(big.Rat).Set(running)
	if metric.plotDivisor != nil && metric.plotDivisor.Sign() != 0 {
		plotted.Quo(plotted, metric.plotDivisor)
	}
	label := formatMetricValue(metric, running)
	return opts.MarkPointNameCoordItem{
		Name:       label,
		Coordinate: []interface{}{rows[historyDays-1].date[5:], ratFloat(plotted)},
		Value:      label,
		Symbol:     "circle",
		SymbolSize: 7,
		ItemStyle: &opts.ItemStyle{
			Color:       "#111111",
			BorderColor: "#111111",
			BorderWidth: 1,
		},
		Label: &opts.Label{
			Show: opts.Bool(false),
		},
	}, true
}

type todayGraphicLabel struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
	Label string  `json:"label"`
}

type endGraphicLabel struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
	Label string  `json:"label"`
}

func todayGraphicLabels(rows []row, metric graphMetric) []todayGraphicLabel {
	daily := totalRows(rows)
	if len(daily) < historyDays {
		return nil
	}
	series := []struct {
		rows []row
	}{
		{rows: daily},
	}
	for _, key := range modelKeysForRows(rows) {
		series = append(series, struct{ rows []row }{rows: modelRows(rows, key)})
	}
	labels := make([]todayGraphicLabel, 0, len(series))
	for _, item := range series {
		if len(item.rows) < historyDays {
			continue
		}
		running := new(big.Rat)
		runningIn := new(big.Rat)
		runningOut := new(big.Rat)
		for i := 0; i < historyDays; i++ {
			running.Add(running, nonNilRat(metric.value(item.rows[i])))
			runningIn.Add(runningIn, tokenInput(item.rows[i].usage))
			runningOut.Add(runningOut, tokenOutput(item.rows[i].usage))
		}
		plotted := new(big.Rat).Set(running)
		if metric.plotDivisor != nil && metric.plotDivisor.Sign() != 0 {
			plotted.Quo(plotted, metric.plotDivisor)
		}
		labels = append(labels, todayGraphicLabel{
			Date:  item.rows[historyDays-1].date[5:],
			Value: ratFloat(plotted),
			Label: formatTotalTokenLabel(running, runningIn, runningOut),
		})
	}
	return labels
}

func formatTotalTokenLabel(total *big.Rat, input *big.Rat, output *big.Rat) string {
	return fmt.Sprintf("%s\n(%s / %s)", formatCommaRat(total), formatCommaRat(input), formatCommaRat(output))
}

func endGraphicLabels(rows []row, metric graphMetric) []endGraphicLabel {
	daily := totalRows(rows)
	if len(daily) == 0 {
		return nil
	}
	series := []struct {
		label string
		rows  []row
	}{
		{label: "TOTAL", rows: daily},
	}
	for _, key := range modelKeysForRows(rows) {
		series = append(series, struct {
			label string
			rows  []row
		}{label: key.provider + "/" + key.model, rows: modelRows(rows, key)})
	}
	labels := make([]endGraphicLabel, 0, len(series))
	for _, item := range series {
		if len(item.rows) == 0 {
			continue
		}
		running := new(big.Rat)
		for _, row := range item.rows {
			running.Add(running, nonNilRat(metric.value(row)))
		}
		plotted := new(big.Rat).Set(running)
		if metric.plotDivisor != nil && metric.plotDivisor.Sign() != 0 {
			plotted.Quo(plotted, metric.plotDivisor)
		}
		last := item.rows[len(item.rows)-1]
		labels = append(labels, endGraphicLabel{
			Date:  last.date[5:],
			Value: ratFloat(plotted),
			Label: item.label,
		})
	}
	return labels
}

func injectChartGraphics(html []byte, yAxisLabel string, todayLabels []todayGraphicLabel, endLabels []endGraphicLabel) ([]byte, error) {
	todayData, err := json.Marshal(todayLabels)
	if err != nil {
		return nil, fmt.Errorf("marshal today labels: %w", err)
	}
	endData, err := json.Marshal(endLabels)
	if err != nil {
		return nil, fmt.Errorf("marshal end labels: %w", err)
	}
	axisLabel, err := json.Marshal(yAxisLabel)
	if err != nil {
		return nil, fmt.Errorf("marshal y-axis label: %w", err)
	}
	script := fmt.Sprintf(`<script type="text/javascript">
setTimeout(function() {
  const chart = goecharts_token_usage_chart;
  const yAxisLabel = %s;
  const todayLabels = %s;
  const endLabels = %s;
  const yAxisLabelWidth = Math.max(72, yAxisLabel.length * 6.6);
  const yAxisLabelLeft = Math.max(8, 82 - yAxisLabelWidth / 2);
  const graphics = [{
    type: 'text',
    left: yAxisLabelLeft,
    top: 72,
    zlevel: 10,
    z: 100,
    silent: true,
    style: {text: yAxisLabel, fill: '#555555', font: '12px Arial, sans-serif', textAlign: 'left', textVerticalAlign: 'middle'}
  }].concat(todayLabels.map(function(item) {
    const point = chart.convertToPixel({xAxisIndex: 0, yAxisIndex: 0}, [item.date, item.value]);
    const lines = item.label.split('\n');
    const width = Math.max(58, Math.max.apply(null, lines.map(function(line) { return line.length; })) * 7.4 + 12);
    const x = Math.max(82, point[0] - width - 12);
    const y = Math.max(96, Math.min(390, point[1] - 34));
	    return {
	      type: 'group',
	      left: x,
	      top: y,
	      zlevel: 10,
      z: 100,
      silent: true,
      children: [
		{type: 'text', style: {x: width - 6, y: 16, text: item.label, fill: '#111111', font: 'bold 12px Arial, sans-serif', textAlign: 'right', textVerticalAlign: 'middle', lineHeight: 14, textBorderColor: '#ffffff', textBorderWidth: 4}}
      ]
    };
  })).concat(endLabels.map(function(item) {
    const point = chart.convertToPixel({xAxisIndex: 0, yAxisIndex: 0}, [item.date, item.value]);
    return {
      type: 'text',
      left: Math.max(92, point[0] - 126),
      top: Math.max(96, Math.min(404, point[1] - 8)),
      zlevel: 10,
      z: 100,
      silent: true,
      style: {x: 120, y: 8, text: item.label, fill: '#111111', font: '12px Arial, sans-serif', textAlign: 'right', textVerticalAlign: 'middle', textBorderColor: '#ffffff', textBorderWidth: 4}
    };
  }));
  chart.setOption({graphic: graphics});
}, 0);
</script>`, string(axisLabel), string(todayData), string(endData))
	text := string(html)
	marker := "</body>"
	if strings.Contains(text, marker) {
		return []byte(strings.Replace(text, marker, script+marker, 1)), nil
	}
	return append(html, []byte(script)...), nil
}

func cumulativeSeries(rows []row, metric graphMetric) []opts.LineData {
	series := make([]opts.LineData, 0, len(rows))
	running := new(big.Rat)
	for _, item := range rows {
		running.Add(running, nonNilRat(metric.value(item)))
		plotted := new(big.Rat).Set(running)
		if metric.plotDivisor != nil && metric.plotDivisor.Sign() != 0 {
			plotted.Quo(plotted, metric.plotDivisor)
		}
		series = append(series, opts.LineData{Value: ratFloat(plotted)})
	}
	return series
}

func modelKeysForRows(rows []row) []usageKey {
	seen := make(map[usageKey]struct{})
	for _, item := range rows {
		if item.provider == "TOTAL" && item.model == "TOTAL" {
			continue
		}
		seen[usageKey{provider: item.provider, model: item.model}] = struct{}{}
	}
	result := make([]usageKey, 0, len(seen))
	for key := range seen {
		result = append(result, key)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].provider != result[j].provider {
			return result[i].provider < result[j].provider
		}
		return result[i].model < result[j].model
	})
	return result
}

func modelRows(rows []row, key usageKey) []row {
	result := make([]row, 0, historyDays+projectionDays)
	for _, item := range rows {
		if item.provider == key.provider && item.model == key.model {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].date < result[j].date })
	return result
}

func localizeECharts(html []byte) ([]byte, func(), error) {
	const assetURL = "https://go-echarts.github.io/go-echarts-assets/assets/echarts.min.js"
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get(assetURL)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch echarts asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("fetch echarts asset: unexpected status %s", resp.Status)
	}
	js, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read echarts asset: %w", err)
	}
	dir, err := os.MkdirTemp("", "trmnl-token-use-assets-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create echarts asset dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	assetPath := filepath.Join(dir, "echarts.min.js")
	if err := os.WriteFile(assetPath, js, 0o600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("write echarts asset: %w", err)
	}
	scriptPattern := regexp.MustCompile(`<script src="[^"]*echarts\.min\.js"></script>`)
	assetURLLocal := (&url.URL{Scheme: "file", Path: filepath.ToSlash(assetPath)}).String()
	replacement := `<script src="` + assetURLLocal + `"></script>`
	result := scriptPattern.ReplaceAllString(string(html), replacement)
	if result == string(html) {
		cleanup()
		return nil, nil, fmt.Errorf("localize echarts asset: script tag not found")
	}
	return []byte(result), cleanup, nil
}

func ratFloat(value *big.Rat) float64 {
	if value == nil {
		return 0
	}
	result, _ := value.Float64()
	return result
}

func screenshotChartHTML(w io.Writer, html []byte) error {
	gray, err := screenshotChartHTMLImage(html)
	if err != nil {
		return err
	}
	return encode1BitPNG(w, gray)
}

func screenshotChartHTMLImage(html []byte) (*image.Gray, error) {
	tmp, err := os.CreateTemp("", "trmnl-token-use-*.html")
	if err != nil {
		return nil, fmt.Errorf("create chart html: %w", err)
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.Write(html); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write chart html: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close chart html: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve chart html: %w", err)
	}

	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(),
		chromedp.ExecPath("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
		chromedp.Headless,
		chromedp.DisableGPU,
		chromedp.NoSandbox,
	)
	defer cancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var pngBytes []byte
	var echartsType string
	pageURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
	if err := chromedp.Run(ctx,
		chromedp.EmulateViewport(820, 500),
		chromedp.Navigate(pageURL),
		chromedp.WaitVisible(`#token_usage_chart`, chromedp.ByQuery),
		chromedp.Evaluate(`typeof echarts`, &echartsType),
		chromedp.ActionFunc(func(context.Context) error {
			if echartsType != "object" {
				return fmt.Errorf("echarts unavailable in browser: typeof echarts is %q", echartsType)
			}
			return nil
		}),
		chromedp.WaitVisible(`#token_usage_chart canvas, #token_usage_chart svg`, chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
		chromedp.Screenshot(`#token_usage_chart`, &pngBytes, chromedp.NodeVisible, chromedp.ByQuery),
	); err != nil {
		return nil, fmt.Errorf("screenshot echarts chart: %w", err)
	}
	return decodeGrayscalePNG(pngBytes)
}

func writeGrayscalePNG(w io.Writer, pngBytes []byte) error {
	gray, err := decodeGrayscalePNG(pngBytes)
	if err != nil {
		return err
	}
	return encode1BitPNG(w, gray)
}

func encode1BitPNG(w io.Writer, gray *image.Gray) error {
	bounds := gray.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	rowBytes := (width + 7) / 8
	raw := make([]byte, 0, height*(rowBytes+1))
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		raw = append(raw, 0) // filter type 0
		row := make([]byte, rowBytes)
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if gray.GrayAt(x, y).Y >= 0xc0 {
				bit := uint(7 - ((x - bounds.Min.X) % 8))
				row[(x-bounds.Min.X)/8] |= 1 << bit
			}
		}
		raw = append(raw, row...)
	}
	var compressed bytes.Buffer
	zw, err := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	if err != nil {
		return fmt.Errorf("create png compressor: %w", err)
	}
	if _, err := zw.Write(raw); err != nil {
		zw.Close()
		return fmt.Errorf("compress png data: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finish png compression: %w", err)
	}
	if _, err := w.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10}); err != nil {
		return fmt.Errorf("write png signature: %w", err)
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], uint32(width))
	binary.BigEndian.PutUint32(ihdr[4:8], uint32(height))
	ihdr[8] = 1 // bit depth
	ihdr[9] = 0 // grayscale
	ihdr[10] = 0
	ihdr[11] = 0
	ihdr[12] = 0
	if err := writePNGChunk(w, "IHDR", ihdr); err != nil {
		return err
	}
	if err := writePNGChunk(w, "IDAT", compressed.Bytes()); err != nil {
		return err
	}
	if err := writePNGChunk(w, "IEND", nil); err != nil {
		return fmt.Errorf("write 2-level grayscale png: %w", err)
	}
	return nil
}

func writePNGChunk(w io.Writer, chunkType string, data []byte) error {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	if _, err := w.Write(length[:]); err != nil {
		return err
	}
	if _, err := io.WriteString(w, chunkType); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	crc := crc32.NewIEEE()
	_, _ = io.WriteString(crc, chunkType)
	_, _ = crc.Write(data)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc.Sum32())
	_, err := w.Write(sum[:])
	return err
}

func decodeGrayscalePNG(pngBytes []byte) (*image.Gray, error) {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode screenshot png: %w", err)
	}
	gray := image.NewGray(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			gray.Set(x, y, color.GrayModel.Convert(img.At(x, y)))
		}
	}
	return gray, nil
}

func drawStatCard(img *image.Gray, rect image.Rectangle, label string, value string) {
	drawRect(img, rect, 0x70)
	fill(img, image.Rect(rect.Min.X+1, rect.Min.Y+1, rect.Max.X-1, rect.Min.Y+16), 0xee)
	drawText(img, rect.Min.X+8, rect.Min.Y+5, label, 0x45, 1)
	drawText(img, rect.Min.X+8, rect.Min.Y+27, value, 0x10, 2)
}

func totalRows(rows []row) []row {
	result := make([]row, 0, historyDays+projectionDays)
	for _, item := range rows {
		if item.provider == "TOTAL" && item.model == "TOTAL" {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].date < result[j].date })
	return result
}

func maxTokenTotal(rows []row) *big.Rat {
	max := new(big.Rat)
	for _, item := range rows {
		total := tokenTotal(item.usage)
		if total.Cmp(max) > 0 {
			max = total
		}
	}
	if max.Sign() == 0 {
		return big.NewRat(1, 1)
	}
	return max
}

func maxCostTotal(rows []row) *big.Rat {
	max := new(big.Rat)
	for _, item := range rows {
		total := new(big.Rat).Add(nonNilRat(item.inCost), nonNilRat(item.outCost))
		if total.Cmp(max) > 0 {
			max = total
		}
	}
	if max.Sign() == 0 {
		return big.NewRat(1, 1)
	}
	return max
}

func tokenTotal(usage usageTotals) *big.Rat {
	total := new(big.Rat)
	total.Add(total, tokenInput(usage))
	total.Add(total, tokenOutput(usage))
	return total
}

func tokenInput(usage usageTotals) *big.Rat {
	return new(big.Rat).Add(nonNilRat(usage.tokenIn), nonNilRat(usage.cachedIn))
}

func tokenOutput(usage usageTotals) *big.Rat {
	return new(big.Rat).Add(nonNilRat(usage.reasoning), nonNilRat(usage.out))
}

func drawTokenBar(img *image.Gray, x int, width int, plot image.Rectangle, usage usageTotals, max *big.Rat) {
	y := plot.Max.Y - 1
	for _, segment := range []struct {
		value *big.Rat
		gray  uint8
	}{
		{usage.tokenIn, 0x2a},
		{usage.cachedIn, 0x7a},
		{usage.reasoning, 0xb0},
		{usage.out, 0x50},
	} {
		h := scaledHeight(segment.value, max, plot.Dy()-2)
		if h <= 0 {
			continue
		}
		fill(img, image.Rect(x, y-h+1, x+width, y+1), segment.gray)
		y -= h
	}
}

func drawCostBar(img *image.Gray, x int, width int, plot image.Rectangle, value *big.Rat, max *big.Rat, shade uint8) {
	h := scaledSqrtHeight(value, max, plot.Dy()-2)
	if h <= 0 {
		return
	}
	fill(img, image.Rect(x, plot.Max.Y-h, x+width, plot.Max.Y), shade)
}

func drawTokenTotalBar(img *image.Gray, x int, width int, plot image.Rectangle, value *big.Rat, max *big.Rat, shade uint8) {
	if shade < 0x80 {
		shade = 0x58
	} else {
		shade = 0xb8
	}
	h := scaledSqrtHeight(value, max, plot.Dy()-2)
	if h <= 0 {
		return
	}
	fill(img, image.Rect(x, plot.Max.Y-h, x+width, plot.Max.Y), shade)
}

func scaledSqrtHeight(value *big.Rat, max *big.Rat, height int) int {
	if value == nil || max == nil || value.Sign() == 0 || max.Sign() == 0 {
		return 0
	}
	ratio := new(big.Rat).Quo(value, max)
	f, _ := ratio.Float64()
	result := int(sqrtApprox(f)*float64(height) + 0.5)
	if result < 1 {
		return 1
	}
	if result > height {
		return height
	}
	return result
}

func scaledHeight(value *big.Rat, max *big.Rat, height int) int {
	return scaledSqrtHeight(value, max, height)
}

func sqrtApprox(value float64) float64 {
	if value <= 0 {
		return 0
	}
	x := value
	if x < 1 {
		x = 1
	}
	for range 8 {
		x = 0.5 * (x + value/x)
	}
	return x
}

func sumCost(rows []row, start int, end int) *big.Rat {
	if start < 0 {
		start = 0
	}
	if start > len(rows) {
		start = len(rows)
	}
	if end > len(rows) {
		end = len(rows)
	}
	total := new(big.Rat)
	for _, item := range rows[start:end] {
		total.Add(total, nonNilRat(item.inCost))
		total.Add(total, nonNilRat(item.outCost))
	}
	return total
}

func formatCompactRat(value *big.Rat) string {
	if value == nil {
		return "0.00"
	}
	return value.FloatString(2)
}

func formatMetricValue(metric graphMetric, value *big.Rat) string {
	if metric.prefix == "$" {
		return metric.prefix + formatCompactRat(value)
	}
	return formatCommaRat(value)
}

func formatCommaRat(value *big.Rat) string {
	if value == nil {
		return "0"
	}
	text := value.FloatString(0)
	negative := strings.HasPrefix(text, "-")
	if negative {
		text = strings.TrimPrefix(text, "-")
	}
	var out []byte
	for i, r := range text {
		if i > 0 && (len(text)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(r))
	}
	if negative {
		return "-" + string(out)
	}
	return string(out)
}

func drawLegend(img *image.Gray, x int, y int) {
	items := []struct {
		label string
		gray  uint8
	}{
		{"IN", 0x2a},
		{"CACHE", 0x7a},
		{"REASON", 0xb0},
		{"OUT", 0x50},
		{"COST", 0x38},
	}
	for i, item := range items {
		yy := y + i*18
		fill(img, image.Rect(x, yy, x+14, yy+10), item.gray)
		drawRect(img, image.Rect(x, yy, x+14, yy+10), 0x20)
		drawText(img, x+22, yy-1, item.label, 0x30, 1)
	}
}

func fill(img *image.Gray, rect image.Rectangle, gray uint8) {
	rect = rect.Intersect(img.Bounds())
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			img.SetGray(x, y, color.Gray{Y: gray})
		}
	}
}

func drawRect(img *image.Gray, rect image.Rectangle, gray uint8) {
	drawHorizontal(img, rect.Min.X, rect.Max.X-1, rect.Min.Y, gray)
	drawHorizontal(img, rect.Min.X, rect.Max.X-1, rect.Max.Y-1, gray)
	drawVertical(img, rect.Min.X, rect.Min.Y, rect.Max.Y-1, gray)
	drawVertical(img, rect.Max.X-1, rect.Min.Y, rect.Max.Y-1, gray)
}

func drawHorizontal(img *image.Gray, x1 int, x2 int, y int, gray uint8) {
	if x2 < x1 {
		x1, x2 = x2, x1
	}
	fill(img, image.Rect(x1, y, x2+1, y+1), gray)
}

func drawVertical(img *image.Gray, x int, y1 int, y2 int, gray uint8) {
	if y2 < y1 {
		y1, y2 = y2, y1
	}
	fill(img, image.Rect(x, y1, x+1, y2+1), gray)
}

func drawText(img *image.Gray, x int, y int, text string, gray uint8, scale int) {
	cursor := x
	for _, r := range strings.ToUpper(text) {
		glyph, ok := font5x7[r]
		if !ok {
			glyph = font5x7['?']
		}
		for rowIndex, row := range glyph {
			for colIndex, pixel := range row {
				if pixel != '1' {
					continue
				}
				fill(img, image.Rect(
					cursor+colIndex*scale,
					y+rowIndex*scale,
					cursor+(colIndex+1)*scale,
					y+(rowIndex+1)*scale,
				), gray)
			}
		}
		cursor += 6 * scale
	}
}

var font5x7 = map[rune][]string{
	' ': {"00000", "00000", "00000", "00000", "00000", "00000", "00000"},
	',': {"00000", "00000", "00000", "00000", "00100", "00100", "01000"},
	'.': {"00000", "00000", "00000", "00000", "00000", "01100", "01100"},
	'-': {"00000", "00000", "00000", "11111", "00000", "00000", "00000"},
	'+': {"00000", "00100", "00100", "11111", "00100", "00100", "00000"},
	'/': {"00001", "00010", "00100", "01000", "10000", "00000", "00000"},
	'$': {"01110", "10100", "10100", "01110", "00101", "00101", "11110"},
	'?': {"11110", "00001", "00010", "00100", "00100", "00000", "00100"},
	'0': {"01110", "10001", "10011", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11110", "00001", "00001", "01110", "00001", "00001", "11110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "10000", "11110", "00001", "00001", "11110"},
	'6': {"01110", "10000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00001", "01110"},
	'A': {"01110", "10001", "10001", "11111", "10001", "10001", "10001"},
	'B': {"11110", "10001", "10001", "11110", "10001", "10001", "11110"},
	'C': {"01110", "10001", "10000", "10000", "10000", "10001", "01110"},
	'D': {"11110", "10001", "10001", "10001", "10001", "10001", "11110"},
	'E': {"11111", "10000", "10000", "11110", "10000", "10000", "11111"},
	'F': {"11111", "10000", "10000", "11110", "10000", "10000", "10000"},
	'G': {"01110", "10001", "10000", "10111", "10001", "10001", "01110"},
	'H': {"10001", "10001", "10001", "11111", "10001", "10001", "10001"},
	'I': {"01110", "00100", "00100", "00100", "00100", "00100", "01110"},
	'J': {"00111", "00010", "00010", "00010", "10010", "10010", "01100"},
	'K': {"10001", "10010", "10100", "11000", "10100", "10010", "10001"},
	'L': {"10000", "10000", "10000", "10000", "10000", "10000", "11111"},
	'M': {"10001", "11011", "10101", "10101", "10001", "10001", "10001"},
	'N': {"10001", "11001", "10101", "10011", "10001", "10001", "10001"},
	'O': {"01110", "10001", "10001", "10001", "10001", "10001", "01110"},
	'P': {"11110", "10001", "10001", "11110", "10000", "10000", "10000"},
	'Q': {"01110", "10001", "10001", "10001", "10101", "10010", "01101"},
	'R': {"11110", "10001", "10001", "11110", "10100", "10010", "10001"},
	'S': {"01111", "10000", "10000", "01110", "00001", "00001", "11110"},
	'T': {"11111", "00100", "00100", "00100", "00100", "00100", "00100"},
	'U': {"10001", "10001", "10001", "10001", "10001", "10001", "01110"},
	'V': {"10001", "10001", "10001", "10001", "10001", "01010", "00100"},
	'W': {"10001", "10001", "10001", "10101", "10101", "10101", "01010"},
	'X': {"10001", "10001", "01010", "00100", "01010", "10001", "10001"},
	'Y': {"10001", "10001", "01010", "00100", "00100", "00100", "00100"},
	'Z': {"11111", "00001", "00010", "00100", "01000", "10000", "11111"},
}
