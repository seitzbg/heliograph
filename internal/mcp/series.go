package mcp

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Round mirrors one row of the /api/series response (internal/api roundDTO). RTTsMs
// gains omitempty here (the API's does not) so compact mode — which nils it out — omits
// the field from the tool's structured output instead of serializing "rtts_ms":null.
type Round struct {
	T        string     `json:"t"`
	MedianMs *float64   `json:"median_ms"`
	Loss     int        `json:"loss"`
	Pings    int        `json:"pings"`
	RTTsMs   []*float64 `json:"rtts_ms,omitempty"`
}

// fetchSeries wraps GET /api/series. from/to (unix ms, both required together) take
// precedence over window when both are given. The API returns rounds oldest->newest;
// when capped to maxRounds, the newest rounds are kept (the tail of that slice).
func fetchSeries(ctx context.Context, c *Client, target, vantage, window string, from, to int64, detail bool, maxRounds int) (string, []Round, error) {
	q := url.Values{}
	q.Set("target", target)
	if vantage != "" {
		q.Set("vantage", vantage)
	}
	switch {
	case from > 0 && to > 0:
		q.Set("from", strconv.FormatInt(from, 10))
		q.Set("to", strconv.FormatInt(to, 10))
	case window != "":
		q.Set("window", window)
	}
	var env struct {
		Metric string  `json:"metric"`
		Rounds []Round `json:"rounds"`
	}
	if err := c.getJSON(ctx, "/api/series", q, &env); err != nil {
		return "", nil, err
	}
	rs := env.Rounds
	if maxRounds > 0 && len(rs) > maxRounds {
		rs = rs[len(rs)-maxRounds:] // keep newest (API returns oldest->newest)
	}
	if !detail {
		for i := range rs {
			rs[i].RTTsMs = nil
		}
	}
	return env.Metric, rs, nil
}

type seriesIn struct {
	Target    string `json:"target" jsonschema:"the target's stable id (from heliograph_status.id); a display path also resolves"`
	Vantage   string `json:"vantage,omitempty" jsonschema:"optional vantage name"`
	Window    string `json:"window,omitempty" jsonschema:"time window as a Go duration, e.g. 3h (ignored if from/to given)"`
	From      int64  `json:"from,omitempty" jsonschema:"optional sub-range start, unix milliseconds (requires to)"`
	To        int64  `json:"to,omitempty" jsonschema:"optional sub-range end, unix milliseconds (requires from)"`
	Detail    bool   `json:"detail,omitempty" jsonschema:"include per-ping rtts_ms bands (token-heavy); default false"`
	MaxRounds int    `json:"max_rounds,omitempty" jsonschema:"cap on returned rounds, newest kept (default 500)"`
}
type seriesOut struct {
	Target string  `json:"target"`
	Metric string  `json:"metric"`
	Rounds []Round `json:"rounds"`
}

func registerSeries(s *sdk.Server, c *Client) {
	sdk.AddTool(s, &sdk.Tool{
		Name:        "heliograph_series",
		Description: "Per-round latency/loss history for one target. Compact by default (t, median, loss, pings); set detail=true for per-ping rtts_ms. Use to inspect a target's trend and confirm/deny a degradation over time.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *sdk.CallToolRequest, in seriesIn) (*sdk.CallToolResult, seriesOut, error) {
		max := in.MaxRounds
		if max == 0 {
			max = 500
		}
		metric, rs, err := fetchSeries(ctx, c, in.Target, in.Vantage, in.Window, in.From, in.To, in.Detail, max)
		if err != nil {
			return nil, seriesOut{}, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "target=%s metric=%s rounds=%d\n", in.Target, metric, len(rs))
		return textResult(b.String()), seriesOut{Target: in.Target, Metric: metric, Rounds: rs}, nil
	})
}
