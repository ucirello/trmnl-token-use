package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Let the CLI handle service discovery and authentication, just as the TUI does.
func queryOpenCodeAPI(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "opencode2", "api", "get", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("run opencode api: %w", ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("run opencode api (ensure opencode is installed and available on PATH): %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

type v2StatsResponse struct {
	Data *struct {
		Range struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"range"`
		Models []struct {
			Model struct {
				ProviderID string `json:"providerID"`
				ID         string `json:"id"`
			} `json:"model"`
			Tokens *struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"models"`
	} `json:"data"`
}

func loadV2Usage(ctx context.Context, window reportWindow, query func(context.Context, string) ([]byte, error)) (map[dailyUsageKey]usageTotals, error) {
	result := make(map[dailyUsageKey]usageTotals)
	// Stats provides model totals for a range, but its daily activity only counts
	// steps. Request each local calendar day to obtain daily token totals.
	for day := window.start; !day.After(window.today); day = day.AddDate(0, 0, 1) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := day.AddDate(0, 0, 1)
		params := url.Values{
			"from":  {strconv.FormatInt(day.UnixMilli(), 10)},
			"to":    {strconv.FormatInt(end.UnixMilli(), 10)},
			"tools": {"none"},
		}
		date := day.Format(time.DateOnly)
		body, err := query(ctx, "/api/experimental/session/stats?"+params.Encode())
		if err != nil {
			return nil, fmt.Errorf("load OpenCode V2 usage for %s: %w", date, err)
		}
		var response v2StatsResponse
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, fmt.Errorf("decode OpenCode V2 usage for %s: %w", date, err)
		}
		data := response.Data
		if data == nil || data.Models == nil {
			return nil, fmt.Errorf("OpenCode V2 usage for %s: missing data or models", date)
		}
		if data.Range.From != day.UnixMilli() || data.Range.To != end.UnixMilli() {
			return nil, fmt.Errorf("OpenCode V2 usage for %s: server returned an unexpected time range", date)
		}
		for _, item := range data.Models {
			if item.Model.ProviderID == "" || item.Model.ID == "" || item.Tokens == nil {
				return nil, fmt.Errorf("OpenCode V2 usage for %s: missing model identity or tokens", date)
			}
			tokens := item.Tokens
			if tokens.Input < 0 || tokens.Output < 0 || tokens.Reasoning < 0 || tokens.Cache.Read < 0 || tokens.Cache.Write < 0 {
				return nil, fmt.Errorf("OpenCode V2 usage for %s: negative token count", date)
			}
			// Variants share a provider/model series, as in the SQLite reports.
			key := dailyUsageKey{date: date, provider: item.Model.ProviderID, model: item.Model.ID}
			result[key] = addUsage(result[key], usageFromInts(tokens.Input, tokens.Cache.Read, tokens.Cache.Write, tokens.Reasoning, tokens.Output))
		}
	}
	return result, nil
}

func loadReportUsage(ctx context.Context, dbPath string, window reportWindow) (map[dailyUsageKey]usageTotals, error) {
	if dbPath == "" {
		return loadV2Usage(ctx, window, queryOpenCodeAPI)
	}
	path, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve opencode database: %w", err)
	}
	// Explicit database reports must never create a database for a mistyped path.
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open opencode database: %w", err)
	}
	defer db.Close()
	return loadUsage(ctx, db, window.start)
}
