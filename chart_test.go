package main

import (
	"testing"
	"time"
)

func TestChartSummaryUsesHistoryAndGroupsOtherModels(t *testing.T) {
	window := reportWindow{start: testDate(2026, 1, 1), today: testDate(2026, 1, 30)}
	usage := make(map[dailyUsageKey]usageTotals)
	for model, tokens := range map[string]int64{"largest": 500, "second": 300, "third": 100, "fourth": 60, "fifth": 40, "unused": 0} {
		usage[dailyUsageKey{date: "2026-01-30", provider: "test", model: model}] = usageFromInts(tokens, 0, 0, 0, 0)
	}
	data := buildChartData(buildRows(usage, nil, window))
	if data.Total != "1.00K" || data.Input != "1.00K" || data.Output != "0" || data.Projected != "1.50K" {
		t.Fatalf("incorrect history/forecast summary: %+v", data)
	}
	if len(data.Models) != 4 {
		t.Fatalf("got %d model rows, want top three plus Other", len(data.Models))
	}
	for i, want := range []string{"largest", "second", "third", "Other (2 models)"} {
		if data.Models[i].Name != want {
			t.Fatalf("model %d: got %q want %q", i, data.Models[i].Name, want)
		}
	}
	if data.Models[3].Share != 10 || data.Models[3].Tokens != "100" {
		t.Fatalf("Other must contain only remaining historical usage: %+v", data.Models[3])
	}
	if data.Today != 29 || len(data.History) != 45 || len(data.Forecast) != 45 {
		t.Fatal("wrong date range")
	}
	if data.History[30] != nil || data.Forecast[28] != nil || *data.History[29] != *data.Forecast[29] {
		t.Fatal("history and forecast must meet at today without overlapping elsewhere")
	}
}

func TestChartModelNamesDisambiguateProviders(t *testing.T) {
	window := newReportWindow(testDate(2026, time.January, 30))
	date := window.today.Format(time.DateOnly)
	data := buildChartData(buildRows(map[dailyUsageKey]usageTotals{
		{date: date, provider: "a", model: "same"}: usageFromInts(10, 0, 0, 0, 0),
		{date: date, provider: "b", model: "same"}: usageFromInts(10, 0, 0, 0, 0),
	}, nil, window))
	if data.Models[0].Name != "a / same" || data.Models[1].Name != "b / same" {
		t.Fatalf("ambiguous model names or unstable tie order: %+v", data.Models)
	}
}

func TestChartEmptyUsage(t *testing.T) {
	data := buildChartData(nil)
	if data.Total != "0" || len(data.Models) != 0 || !data.Empty {
		t.Fatalf("incorrect empty state: %+v", data)
	}
	if len(data.Dates) != historyDays+projectionDays || data.Today != historyDays-1 {
		t.Fatal("empty chart must retain its calendar range")
	}
}
