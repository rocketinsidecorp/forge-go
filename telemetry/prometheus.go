package telemetry

import (
	"bytes"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// PrometheusContentType is the media type of the Prometheus text format.
const PrometheusContentType = "text/plain; version=0.0.4; charset=utf-8"

// WritePrometheus writes every instrument in the Prometheus text exposition
// format 0.0.4. Names are converted from OpenTelemetry style: dots become
// underscores, the unit becomes a suffix ("s" adds _seconds, "By" _bytes)
// and counters end in _total. http.server.request.duration is exposed as
// http_server_request_duration_seconds.
func (r *Registry) WritePrometheus(w io.Writer) error {
	out := &bytes.Buffer{}
	for _, metric := range r.Collect() {
		name := PrometheusName(metric.Name, metric.Unit, metric.Kind)
		kind := "gauge"
		switch metric.Kind {
		case MetricCounter:
			kind = "counter"
		case MetricHistogram:
			kind = "histogram"
		}
		if metric.Description != "" {
			out.WriteString("# HELP " + name + " " + escapeHelp(metric.Description) + "\n")
		}
		out.WriteString("# TYPE " + name + " " + kind + "\n")
		for _, p := range metric.Points {
			if metric.Kind != MetricHistogram {
				writeSample(out, name, p.Labels, "", "", p.Value)
				continue
			}
			var cumulative uint64
			for i, count := range p.BucketCounts {
				cumulative += count
				le := "+Inf"
				if i < len(metric.Bounds) {
					le = formatFloat(metric.Bounds[i])
				}
				writeSample(out, name+"_bucket", p.Labels, "le", le, float64(cumulative))
			}
			writeSample(out, name+"_sum", p.Labels, "", "", p.Sum)
			writeSample(out, name+"_count", p.Labels, "", "", float64(p.Count))
		}
	}
	_, err := w.Write(out.Bytes())
	return err
}

// PrometheusHandler serves the registry for Prometheus scrapes. It answers
// GET and HEAD only and is meant for the separate metrics listener, never
// the public application router (see docs/OBSERVABILITY.md).
func (r *Registry) PrometheusHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body bytes.Buffer
		if err := r.WritePrometheus(&body); err != nil {
			http.Error(w, "metrics unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", PrometheusContentType)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
		if req.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body.Bytes())
	})
}

// PrometheusName converts an OpenTelemetry metric name and unit to a
// Prometheus metric name.
func PrometheusName(name, unit string, kind MetricKind) string {
	result := sanitizePrometheus(name)
	if suffix := unitSuffix(unit); suffix != "" && !strings.HasSuffix(result, "_"+suffix) {
		result += "_" + suffix
	}
	if kind == MetricCounter && !strings.HasSuffix(result, "_total") {
		result += "_total"
	}
	return result
}

func unitSuffix(unit string) string {
	switch unit {
	case "s":
		return "seconds"
	case "ms":
		return "milliseconds"
	case "By":
		return "bytes"
	}
	return ""
}

func sanitizePrometheus(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for i := range len(name) {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9' && i > 0) || c == '_' || c == ':' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func writeSample(out *bytes.Buffer, name string, labels []Attribute, extraName, extraValue string, value float64) {
	out.WriteString(name)
	if len(labels) > 0 || extraName != "" {
		out.WriteByte('{')
		for i, label := range labels {
			if i > 0 {
				out.WriteByte(',')
			}
			out.WriteString(sanitizePrometheus(label.Key))
			out.WriteString(`="`)
			out.WriteString(escapeLabelValue(label.Value.AsString()))
			out.WriteByte('"')
		}
		if extraName != "" {
			if len(labels) > 0 {
				out.WriteByte(',')
			}
			out.WriteString(extraName + `="` + extraValue + `"`)
		}
		out.WriteByte('}')
	}
	out.WriteByte(' ')
	out.WriteString(formatFloat(value))
	out.WriteByte('\n')
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabelValue(v string) string {
	if !utf8.ValidString(v) {
		v = strings.ToValidUTF8(v, "�")
	}
	return labelEscaper.Replace(v)
}

var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeHelp(v string) string { return helpEscaper.Replace(v) }
