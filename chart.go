package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/opts"
)

type chartModel struct {
	Name   string  `json:"name"`
	Tokens string  `json:"tokens"`
	Share  float64 `json:"share"`
}

type chartData struct {
	Total     string       `json:"total"`
	Input     string       `json:"input"`
	Output    string       `json:"output"`
	Projected string       `json:"projected"`
	Unit      string       `json:"unit"`
	Dates     []string     `json:"dates"`
	History   []*float64   `json:"history"`
	Forecast  []*float64   `json:"forecast"`
	Today     int          `json:"today"`
	Models    []chartModel `json:"models"`
	Empty     bool         `json:"empty"`
}

func compactTokens(value *big.Rat) string {
	if value == nil || value.Sign() == 0 {
		return "0"
	}
	divisor, unit := tokenScale(value)
	if unit == "" {
		return value.FloatString(0)
	}
	return new(big.Rat).Quo(value, divisor).FloatString(2) + unit
}

func tokenScale(value *big.Rat) (*big.Rat, string) {
	for _, unit := range []struct {
		divisor int64
		name    string
	}{{1_000_000_000_000, "T"}, {1_000_000_000, "B"}, {1_000_000, "M"}, {1_000, "K"}} {
		divisor := big.NewRat(unit.divisor, 1)
		if nonNilRat(value).Cmp(divisor) >= 0 {
			return divisor, unit.name
		}
	}
	return big.NewRat(1, 1), ""
}

func buildChartData(rows []row) chartData {
	daily := totalRows(rows)
	if len(daily) == 0 {
		window := newReportWindow(time.Now())
		for i := 0; i < historyDays+projectionDays; i++ {
			daily = append(daily, row{date: window.start.AddDate(0, 0, i).Format(time.DateOnly)})
		}
	}
	today := min(historyDays, len(daily)) - 1
	historical := usageTotals{}
	projected := new(big.Rat)
	for i, item := range daily {
		projected.Add(projected, tokenTotal(item.usage))
		if i <= today {
			historical = addUsage(historical, item.usage)
		}
	}
	total := tokenTotal(historical)
	divisor, unit := tokenScale(projected)
	data := chartData{
		Total: compactTokens(total), Input: compactTokens(tokenInput(historical)),
		Output: compactTokens(tokenOutput(historical)), Projected: compactTokens(projected),
		Unit: unit + " tokens", Today: today, Empty: total.Sign() == 0,
		History: make([]*float64, len(daily)), Forecast: make([]*float64, len(daily)),
		Models: make([]chartModel, 0, 4),
	}
	running := new(big.Rat)
	for i, item := range daily {
		date, _ := time.Parse(time.DateOnly, item.date)
		data.Dates = append(data.Dates, date.Format("Jan 02"))
		running.Add(running, tokenTotal(item.usage))
		value := ratFloat(new(big.Rat).Quo(running, divisor))
		if i <= today {
			data.History[i] = &value
		}
		if i >= today {
			data.Forecast[i] = &value
		}
	}

	// Model shares use only the measured history, never the projected rows or TOTAL.
	byModel := make(map[usageKey]*big.Rat)
	for _, item := range rows {
		if (item.provider == "TOTAL" && item.model == "TOTAL") || item.date < daily[0].date || item.date > daily[today].date {
			continue
		}
		key := usageKey{provider: item.provider, model: item.model}
		if byModel[key] == nil {
			byModel[key] = new(big.Rat)
		}
		byModel[key].Add(byModel[key], tokenTotal(item.usage))
	}
	keys := make([]usageKey, 0, len(byModel))
	for key, tokens := range byModel {
		if tokens.Sign() > 0 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if order := byModel[keys[i]].Cmp(byModel[keys[j]]); order != 0 {
			return order > 0
		}
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].model < keys[j].model
	})
	addModel := func(name string, tokens *big.Rat) {
		share := 0.0
		if total.Sign() > 0 {
			share = ratFloat(new(big.Rat).Quo(tokens, total)) * 100
		}
		data.Models = append(data.Models, chartModel{Name: name, Tokens: compactTokens(tokens), Share: share})
	}
	names := make(map[string]int)
	for _, key := range keys[:min(3, len(keys))] {
		names[key.model]++
	}
	other := new(big.Rat)
	for i, key := range keys {
		if i >= 3 {
			other.Add(other, byModel[key])
			continue
		}
		name := key.model
		if names[name] > 1 {
			name = key.provider + " / " + name
		}
		addModel(name, byModel[key])
	}
	if len(keys) > 3 {
		label := fmt.Sprintf("Other (%d models)", len(keys)-3)
		if len(keys) == 4 {
			label = "Other (1 model)"
		}
		addModel(label, other)
	}
	return data
}

func writePNG(w io.Writer, rows []row) error {
	line := charts.NewLine()
	line.SetGlobalOptions(charts.WithInitializationOpts(opts.Initialization{
		ChartID: "token_usage_chart", Width: "800px", Height: "480px", Renderer: "svg",
	}))
	var html bytes.Buffer
	if err := line.Render(&html); err != nil {
		return fmt.Errorf("render echarts html: %w", err)
	}
	localized, cleanup, err := localizeECharts(html.Bytes())
	if err != nil {
		return err
	}
	defer cleanup()
	page, err := injectDashboard(localized, buildChartData(rows))
	if err != nil {
		return err
	}
	return screenshotChartHTML(w, page)
}

func injectDashboard(html []byte, data chartData) ([]byte, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshal dashboard: %w", err)
	}
	now := time.Now().Local().Format("15:04")
	script := `<script>
(function() {
  const data = ` + string(payload) + `;
  const chart = goecharts_token_usage_chart;
  function text(x, y, value, size, bold, align, width) {
    return {type: 'text', silent: true, z: 10,
      style: {x: x, y: y, text: value, fill: '#000', font: (bold ? 'bold ' : '') + size + 'px Arial, sans-serif',
        align: align || 'left', verticalAlign: 'top', width: width, overflow: 'truncate', ellipsis: '...'}};
  }
  const graphics = [
    text(24, 16, 'TOKEN USAGE', 13, true),
    text(24, 38, data.total, 42, true),
    text(218, 59, 'tokens / last 30 days', 15, false),
    text(24, 90, 'Input ' + data.input + '   /   Output ' + data.output, 14, false),
    text(776, 16, 'OPENCODE', 12, true, 'right'),
    text(776, 43, data.projected, 27, true, 'right'),
    text(776, 77, 'projected total in 15 days', 12, false, 'right'),
    text(24, 119, data.unit.trim(), 12, false),
    text(24, 342, 'MODEL SHARE', 12, true),
    text(776, 342, 'LAST 30 DAYS', 11, false, 'right')
  ];
  data.models.forEach(function(model, index) {
    const y = 366 + index * 26;
    graphics.push(text(24, y, model.name, 14, false, 'left', 210));
    graphics.push({type: 'rect', silent: true, shape: {x: 250, y: y + 1, width: 336, height: 12}, style: {fill: '#fff', stroke: '#000', lineWidth: 1}});
    graphics.push({type: 'rect', silent: true, shape: {x: 250, y: y + 1, width: 336 * model.share / 100, height: 12}, style: {fill: '#000'}});
    graphics.push(text(698, y, model.tokens, 14, true, 'right'));
    const share = model.share < 0.1 ? '<0.1%' : model.share.toFixed(1) + '%';
    graphics.push(text(776, y, share, 14, false, 'right'));
  });
  if (data.empty) {
    graphics.push(text(420, 204, 'No usage in the last 30 days', 17, true, 'center'));
    graphics.push(text(24, 365, 'No model usage recorded', 14, false));
  }
  chart.setOption({
    animation: false, backgroundColor: '#fff', graphic: graphics,
    grid: {left: 64, right: 26, top: 144, bottom: 184},
    xAxis: {type: 'category', boundaryGap: false, data: data.dates,
      axisLine: {lineStyle: {color: '#000'}}, axisTick: {show: false},
      axisLabel: {interval: function(index) {
        return (index % 7 === 0 && index < data.dates.length - 4) || index === data.dates.length - 1;
      }, showMaxLabel: true, color: '#000', fontSize: 12, margin: 12}},
    yAxis: {type: 'value', min: 0, splitNumber: 3, minInterval: 1,
      axisLine: {show: false}, axisTick: {show: false},
      axisLabel: {color: '#000', fontSize: 12},
      splitLine: {lineStyle: {color: '#999', type: 'dotted'}}},
    series: [
      {name: 'History', type: 'line', data: data.history, showSymbol: false, connectNulls: false,
        lineStyle: {color: '#000', width: 3},
        markLine: {silent: true, symbol: ['none', 'none'],
          lineStyle: {color: '#000', width: 1, type: 'dotted'},
          label: {formatter: '` + now + `', position: 'end', color: '#000', fontSize: 12},
          data: data.empty ? [] : [{xAxis: data.today}]},
        markPoint: {silent: true, symbol: 'circle', symbolSize: 7,
          itemStyle: {color: '#000'}, label: {show: false},
          data: data.empty ? [] : [{coord: [data.today, data.history[data.today]]}]}},
      {name: 'Forecast', type: 'line', data: data.forecast, showSymbol: false, connectNulls: false,
        lineStyle: {color: '#000', width: 2, type: 'dashed'}}
    ]
  }, true);
  window.trmnlChartReady = true;
})();
</script>`
	return []byte(strings.Replace(string(html), "</body>", script+"</body>", 1)), nil
}
