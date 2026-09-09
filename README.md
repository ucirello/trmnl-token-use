# TRMNL token usage

Show your OpenCode usage on TRMNL: 30 days of token history and a 15-day
projection based on the historical daily average, grouped by provider and model.

The PNG shows a large 30-day total with input/output counts, a single cumulative
line with a dashed forecast and a Today marker, and weekly date labels. Below it,
horizontal bars show the top three provider/model groups plus the remaining usage
as Other. Model shares use historical usage only. CSV retains the full breakdown.

## Requirements

- Go 1.26.1 or newer to build/run.
- OpenCode V2 (`opencode2`) on `PATH`, with access to your OpenCode service.
- Google Chrome at `/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`
  for PNG rendering.
- Internet access for models.dev prices and the ECharts rendering asset.

## Usage

```sh
# Render locally using OpenCode V2 usage.
go run . render -out token-usage.png

# CSV includes daily tokens and estimated costs.
go run . render -format csv > token-usage.csv

# Send one image to your configured TRMNL Webhook Image URL.
export TRMNL_TOKEN_USAGE_WEBHOOK='your-webhook-image-url'
go run . ship

# Run continuously, uploading at :05, :25, and :45 each hour.
go run .
```

The default source is `opencode2 api get /api/experimental/session/stats`. The CLI handles
service discovery and authentication. The report requests each local calendar
day separately, across projects, and combines model variants into one series.
It uses V2's accounting for migrated history and subagents. Failed requests abort
the report rather than uploading partial totals. Daemon mode retries at the next
scheduled time.

For an explicit SQLite report instead:

```sh
go run . render -db /path/to/opencode.db -format csv
```

The database is opened read-only. The reader supports the legacy `message` and
newer `session_message` layouts; when both contain the same message ID, the newer
record wins. This option reads only that database and does not merge in API data.

Costs are estimates using current models.dev prices, not OpenCode's recorded
charges. Missing prices currently produce zero estimated cost. The existing
chart counts input + cache-read + output + reasoning tokens; cache-write tokens
are included in input cost but not in the plotted token total.

Run `go run . --help` for all flags, or `go test ./...` to run the tests (including
a Chrome-based PNG rendering check).
