package overview

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/coroot/coroot/utils"

	"github.com/coroot/coroot/timeseries"

	"golang.org/x/exp/maps"

	"k8s.io/klog"

	"github.com/coroot/coroot/clickhouse"
	"github.com/coroot/coroot/model"
)

const (
	spansLimit      = 100
	attrValuesLimit = 10
)

type Traces struct {
	Message   string                     `json:"message"`
	Error     string                     `json:"error"`
	Heatmap   *model.Heatmap             `json:"heatmap"`
	Spans     []Span                     `json:"spans"`
	Limit     int                        `json:"limit"`
	Summary   *model.TraceSpanSummary    `json:"summary"`
	AttrStats []model.TraceSpanAttrStats `json:"attr_stats"`
	Errors    []model.TraceErrorsStat    `json:"errors"`
}

type Span struct {
	Service    string                 `json:"service"`
	TraceId    string                 `json:"trace_id"`
	Id         string                 `json:"id"`
	ParentId   string                 `json:"parent_id"`
	Name       string                 `json:"name"`
	Timestamp  int64                  `json:"timestamp"`
	Duration   float64                `json:"duration"`
	Status     model.TraceSpanStatus  `json:"status"`
	Details    model.TraceSpanDetails `json:"details"`
	Attributes map[string]string      `json:"attributes"`
	Events     []Event                `json:"events"`
}

type Event struct {
	Timestamp  int64             `json:"timestamp"`
	Name       string            `json:"name"`
	Attributes map[string]string `json:"attributes"`
}

type Query struct {
	View    string          `json:"view"`
	TsFrom  timeseries.Time `json:"ts_from"`
	TsTo    timeseries.Time `json:"ts_to"`
	DurFrom string          `json:"dur_from"`
	DurTo   string          `json:"dur_to"`

	TraceId     string `json:"trace_id"`
	ServiceName string `json:"service_name"`
	SpanName    string `json:"span_name"`
	IncludeAux  bool   `json:"include_aux"`

	durFrom time.Duration
	durTo   time.Duration
	errors  bool
}

func renderTraces(ctx context.Context, ch *clickhouse.Client, w *model.World, query string) *Traces {
	res := &Traces{}

	if ch == nil {
		res.Message = "no_clickhouse"
		return res
	}

	q := parseQuery(query, w.Ctx)

	sq := clickhouse.SpanQuery{
		Ctx:         w.Ctx,
		ServiceName: q.ServiceName,
		SpanName:    q.SpanName,
	}
	if !q.IncludeAux {
		sq.ExcludePeerAddrs = getMonitoringAndControlPlanePodIps(w)
	}

	histogram, err := ch.GetRootSpansHistogram(ctx, sq)
	if err != nil {
		klog.Errorln(err)
		res.Error = fmt.Sprintf("Clickhouse error: %s", err)
		return res
	}
	if len(histogram) > 1 {
		res.Heatmap = model.NewHeatmap(w.Ctx, "Latency & Errors heatmap, requests per second")
		for _, h := range model.HistogramSeries(histogram[1:], 0, 0) {
			res.Heatmap.AddSeries(h.Name, h.Title, h.Data, h.Threshold, h.Value)
		}
		res.Heatmap.AddSeries("errors", "errors", histogram[0].TimeSeries, "", "err")
	} else {
		res.Message = "not_found"
		return res
	}

	sq.TsFrom = q.TsFrom
	if sq.TsFrom == 0 {
		sq.TsFrom = sq.Ctx.From
	}
	sq.TsTo = q.TsTo
	if sq.TsTo == 0 {
		sq.TsTo = sq.Ctx.To
	}
	sq.DurFrom = q.durFrom
	sq.DurTo = q.durTo
	sq.Errors = q.errors
	sq.Limit = spansLimit

	var spans []*model.TraceSpan
	switch {
	case q.TraceId != "":
		spans, err = ch.GetSpansByTraceId(ctx, q.TraceId)
	case q.View == "traces":
		spans, err = ch.GetRootSpans(ctx, sq)
	case q.View == "attributes":
		sq.Limit = attrValuesLimit
		res.AttrStats, err = ch.GetSpanAttrStats(ctx, sq)
	case q.View == "errors":
		res.Errors, err = ch.GetTraceErrors(ctx, sq)
	default:
		res.Summary, err = ch.GetRootSpansSummary(ctx, sq)
	}

	if err != nil {
		klog.Errorln(err)
		res.Error = fmt.Sprintf("Clickhouse error: %s", err)
		return res
	}

	if len(spans) == spansLimit {
		res.Limit = spansLimit
	}

	for _, s := range spans {
		ss := Span{
			Service:    s.ServiceName,
			TraceId:    s.TraceId,
			Id:         s.SpanId,
			ParentId:   s.ParentSpanId,
			Name:       s.Name,
			Timestamp:  s.Timestamp.UnixMilli(),
			Duration:   s.Duration.Seconds() * 1000,
			Status:     s.Status(),
			Attributes: map[string]string{},
			Details:    s.Details(),
		}
		for name, value := range s.ResourceAttributes {
			ss.Attributes[name] = value
		}
		for name, value := range s.SpanAttributes {
			ss.Attributes[name] = value
		}
		for _, e := range s.Events {
			ss.Events = append(ss.Events, Event{
				Timestamp:  e.Timestamp.UnixMilli(),
				Name:       e.Name,
				Attributes: e.Attributes,
			})
		}
		res.Spans = append(res.Spans, ss)
	}

	return res
}

func getMonitoringAndControlPlanePodIps(w *model.World) []string {
	res := map[string]bool{}
	for _, a := range w.Applications {
		if a.Category.Monitoring() || a.Category.ControlPlane() {
			for _, i := range a.Instances {
				for l := range i.TcpListens {
					if ip := net.ParseIP(l.IP); ip != nil && !ip.IsLoopback() {
						res[l.IP] = true
					}
				}
			}
		}
	}
	return maps.Keys(res)
}

func parseQuery(query string, ctx timeseries.Context) Query {
	var res Query
	if query != "" {
		if err := json.Unmarshal([]byte(query), &res); err != nil {
			klog.Warningln(err)
		}
	}
	res.durFrom = utils.ParseHeatmapDuration(res.DurFrom)
	res.durTo = utils.ParseHeatmapDuration(res.DurTo)
	res.errors = res.DurFrom == "inf" || res.DurTo == "err"
	return res
}
