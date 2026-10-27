package telemetry

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// PrettyOptions configure NewPrettyHandler.
type PrettyOptions struct {
	// Color writes ANSI colors. Callers enable it only for a terminal and
	// only when NO_COLOR is unset (forge.Run does).
	Color bool
	// Level is the lowest level written. Default slog.LevelInfo.
	Level slog.Leveler
	// ReplaceAttr is slog.HandlerOptions.ReplaceAttr: it runs on the message
	// and on every attribute, group members included, as the standard
	// handlers do (see Redactor).
	ReplaceAttr func(groups []string, a slog.Attr) slog.Attr
	// Now stamps records that carry no time. Default time.Now.
	Now func() time.Time
}

// NewPrettyHandler returns the development handler: one line per record,
// laid out so that a request can be followed by eye. Request, SQL and job
// events (LogEventHTTP, LogEventSQL and the worker's job outcomes) have their
// own layout, other records read "time LEVEL message key=value":
//
//	12:04:05.123  HTTP  GET /posts/{id}  200  4.1ms  queries=3  req=0197...
//	              SQL   2.3ms  SELECT id, title FROM posts WHERE id = $1
//	12:04:05.130  JOB   send_welcome succeeded  35ms  queue=default attempt=1
//	12:04:05.131  INFO  serving HTTP  address=127.0.0.1:8080
//
// Control characters in messages and values are escaped, so input cannot
// forge a line or send terminal escape sequences. The handler is safe for
// concurrent use.
func NewPrettyHandler(w io.Writer, opts PrettyOptions) slog.Handler {
	if opts.Level == nil {
		opts.Level = slog.LevelInfo
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &prettyHandler{out: &prettyOutput{w: w, opts: opts}}
}

// NewPrettyOutputHandler is NewPrettyHandler as a standard-output handler:
// wrapped by NewLogHandler for trace correlation, and reporting its
// ReplaceAttr and the text format to the log exporter (LogOutputHandler).
func NewPrettyOutputHandler(w io.Writer, opts PrettyOptions) slog.Handler {
	output := LogOutput{Format: LogFormatText, ReplaceAttr: opts.ReplaceAttr}
	return NewLogHandler(outputHandler{Handler: NewPrettyHandler(w, opts), output: output})
}

type prettyOutput struct {
	mu   sync.Mutex
	w    io.Writer
	opts PrettyOptions
}

type prettyAttr struct {
	key   string
	value slog.Value
}

type prettyHandler struct {
	out    *prettyOutput
	attrs  []prettyAttr
	groups []string
}

func (h *prettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.out.opts.Level.Level()
}

func (h *prettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := &prettyHandler{out: h.out, groups: h.groups, attrs: append([]prettyAttr(nil), h.attrs...)}
	for _, attr := range attrs {
		next.attrs = h.out.flatten(next.attrs, h.groups, attr, true)
	}
	return next
}

func (h *prettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &prettyHandler{out: h.out, attrs: h.attrs, groups: append(append([]string(nil), h.groups...), name)}
}

// flatten appends attr to dst as leaves with dotted keys, after ReplaceAttr
// (replace false for the members of a group ReplaceAttr itself returned).
func (o *prettyOutput) flatten(dst []prettyAttr, groups []string, attr slog.Attr, replace bool) []prettyAttr {
	attr.Value = attr.Value.Resolve()
	if attr.Value.Kind() == slog.KindGroup {
		members := attr.Value.Group()
		if len(members) == 0 {
			return dst
		}
		if attr.Key != "" {
			groups = append(append([]string(nil), groups...), attr.Key)
		}
		for _, member := range members {
			dst = o.flatten(dst, groups, member, replace)
		}
		return dst
	}
	if replace && o.opts.ReplaceAttr != nil {
		attr = o.opts.ReplaceAttr(groups, attr)
		attr.Value = attr.Value.Resolve()
		if attr.Value.Kind() == slog.KindGroup {
			return o.flatten(dst, groups, attr, false)
		}
	}
	if attr.Key == "" && attr.Value.Kind() == slog.KindAny && attr.Value.Any() == nil {
		return dst
	}
	key := attr.Key
	if len(groups) > 0 {
		key = strings.Join(groups, ".") + "." + key
	}
	return append(dst, prettyAttr{key: key, value: attr.Value})
}

func (h *prettyHandler) Handle(_ context.Context, record slog.Record) error {
	o := h.out
	attrs := append(make([]prettyAttr, 0, len(h.attrs)+record.NumAttrs()), h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = o.flatten(attrs, h.groups, attr, true)
		return true
	})
	message := record.Message
	if o.opts.ReplaceAttr != nil {
		message = o.opts.ReplaceAttr(nil, slog.String(slog.MessageKey, message)).Value.String()
	}
	when := record.Time
	if when.IsZero() {
		when = o.opts.Now()
	}
	line := o.render(when, record.Level, message, attrs)
	o.mu.Lock()
	defer o.mu.Unlock()
	_, err := io.WriteString(o.w, line)
	return err
}

const (
	ansiReset   = "\x1b[0m"
	ansiDim     = "2"
	ansiRed     = "31"
	ansiGreen   = "32"
	ansiYellow  = "33"
	ansiBlue    = "34"
	ansiMagenta = "35"
	ansiCyan    = "36"
	ansiBold    = "1"
)

func (o *prettyOutput) paint(code, text string) string {
	if !o.opts.Color || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + ansiReset
}

// timeWidth is the width of the timestamp column: SQL lines of a request
// leave it blank, so they read as the request's body.
const timeWidth = len("15:04:05.000")

func (o *prettyOutput) render(when time.Time, level slog.Level, message string, attrs []prettyAttr) string {
	var line strings.Builder
	stamp := func() { line.WriteString(o.paint(ansiDim, when.Format("15:04:05.000"))) }
	switch message {
	case LogEventHTTP:
		if rest, ok := o.renderHTTP(&line, stamp, attrs); ok {
			o.writeAttrs(&line, rest)
			return line.String() + "\n"
		}
	case LogEventSQL:
		if rest, ok := o.renderSQL(&line, stamp, attrs); ok {
			o.writeAttrs(&line, rest)
			return line.String() + "\n"
		}
	default:
		if rest, ok := o.renderJob(&line, stamp, attrs); ok {
			o.writeAttrs(&line, rest)
			return line.String() + "\n"
		}
	}
	line.Reset()
	stamp()
	line.WriteString("  ")
	line.WriteString(o.levelTag(level))
	line.WriteString("  ")
	line.WriteString(clean(message))
	o.writeAttrs(&line, attrs)
	return line.String() + "\n"
}

// levelTag returns the four-character, colored name of level.
func (o *prettyOutput) levelTag(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return o.paint(ansiRed, "ERR ")
	case level >= slog.LevelWarn:
		return o.paint(ansiYellow, "WARN")
	case level >= slog.LevelInfo:
		return o.paint(ansiGreen, "INFO")
	}
	return o.paint(ansiDim, "DBG ")
}

// take removes the first attribute named key from attrs and returns it.
func take(attrs []prettyAttr, key string) (slog.Value, []prettyAttr, bool) {
	for i, attr := range attrs {
		if attr.key == key {
			return attr.value, append(append([]prettyAttr(nil), attrs[:i]...), attrs[i+1:]...), true
		}
	}
	return slog.Value{}, attrs, false
}

// drop removes every attribute named in keys.
func drop(attrs []prettyAttr, keys ...string) []prettyAttr {
	kept := make([]prettyAttr, 0, len(attrs))
next:
	for _, attr := range attrs {
		for _, key := range keys {
			if attr.key == key {
				continue next
			}
		}
		kept = append(kept, attr)
	}
	return kept
}

func (o *prettyOutput) renderHTTP(line *strings.Builder, stamp func(), attrs []prettyAttr) ([]prettyAttr, bool) {
	method, attrs, ok1 := take(attrs, "method")
	status, attrs, ok2 := take(attrs, "status")
	duration, attrs, ok3 := take(attrs, "duration")
	if !ok1 || !ok2 || !ok3 || status.Kind() != slog.KindInt64 || duration.Kind() != slog.KindDuration {
		return nil, false
	}
	route, attrs, _ := take(attrs, "route")
	queries, attrs, hasQueries := take(attrs, "queries")
	request, attrs, hasRequest := take(attrs, "request_id")
	attrs = drop(attrs, LogTraceIDKey, LogSpanIDKey)

	stamp()
	line.WriteString("  " + o.paint(ansiBlue, "HTTP") + "  ")
	// A pattern that matches the path exactly ("/{$}") reads as the path.
	routeText := clean(strings.ReplaceAll(route.String(), "{$}", ""))
	if routeText == "" {
		routeText = "-"
	}
	line.WriteString(clean(method.String()) + " " + routeText + "  ")
	code := int(status.Int64())
	line.WriteString(o.paint(statusColor(code), strconv.Itoa(code)))
	line.WriteString("  " + formatDuration(duration.Duration()))
	if hasQueries {
		line.WriteString("  queries=" + formatValue(queries))
	}
	if hasRequest {
		line.WriteString("  " + o.paint(ansiDim, "req="+clean(request.String())))
	}
	return attrs, true
}

// statusColor is green for 2xx, cyan for 3xx, yellow for 4xx and red for 5xx.
func statusColor(status int) string {
	switch {
	case status >= 500:
		return ansiRed
	case status >= 400:
		return ansiYellow
	case status >= 300:
		return ansiCyan
	case status >= 200:
		return ansiGreen
	}
	return ansiDim
}

func (o *prettyOutput) renderSQL(line *strings.Builder, stamp func(), attrs []prettyAttr) ([]prettyAttr, bool) {
	statement, attrs, ok1 := take(attrs, "statement")
	duration, attrs, ok2 := take(attrs, "duration")
	if !ok1 || !ok2 || duration.Kind() != slog.KindDuration {
		return nil, false
	}
	slow, attrs, _ := take(attrs, "slow")
	failure, attrs, failed := take(attrs, "error")
	_, attrs, inRequest := take(attrs, "request_id")
	attrs = drop(attrs, LogTraceIDKey, LogSpanIDKey)

	if inRequest {
		line.WriteString(strings.Repeat(" ", timeWidth))
	} else {
		stamp()
	}
	line.WriteString("  " + o.paint(ansiMagenta, "SQL ") + "  ")
	text := formatDuration(duration.Duration())
	switch {
	case failed:
		text = o.paint(ansiRed, text)
	case slow.Kind() == slog.KindBool && slow.Bool():
		text = o.paint(ansiYellow, text)
	}
	line.WriteString(text + "  " + clean(statement.String()))
	if failed {
		line.WriteString("  " + o.paint(ansiRed, "error="+clean(failure.String())))
	}
	return attrs, true
}

// renderJob lays out the worker's outcome records (queue.Worker): a job id,
// a kind, an outcome and a duration.
func (o *prettyOutput) renderJob(line *strings.Builder, stamp func(), attrs []prettyAttr) ([]prettyAttr, bool) {
	kind, attrs, ok1 := take(attrs, "kind")
	outcome, attrs, ok2 := take(attrs, "outcome")
	duration, attrs, ok3 := take(attrs, "duration")
	id, attrs, ok4 := take(attrs, "job_id")
	if !ok1 || !ok2 || !ok3 || !ok4 || duration.Kind() != slog.KindDuration {
		return nil, false
	}
	retry, attrs, _ := take(attrs, "retry_in")
	failure, attrs, failed := take(attrs, "error")
	attrs = drop(attrs, "component", "worker_id", "max_attempts", LogTraceIDKey, LogSpanIDKey)

	label, color := outcome.String(), ansiYellow
	switch label {
	case "succeeded":
		color = ansiGreen
	case "retry":
		label, color = "failed, retrying", ansiRed
		if retry.Kind() == slog.KindDuration {
			label = "failed, retry in " + formatDuration(retry.Duration())
		}
	case "dead":
		label, color = "failed permanently", ansiRed
	}
	stamp()
	line.WriteString("  " + o.paint(ansiCyan, "JOB ") + "  ")
	line.WriteString(clean(kind.String()) + " " + o.paint(color, clean(label)))
	line.WriteString("  " + formatDuration(duration.Duration()))
	if color == ansiRed {
		attrs = append(attrs, prettyAttr{key: "job_id", value: id})
		if failed {
			attrs = append(attrs, prettyAttr{key: "error", value: failure})
		}
	}
	return attrs, true
}

// writeAttrs writes key=value pairs, two spaces after what precedes them.
func (o *prettyOutput) writeAttrs(line *strings.Builder, attrs []prettyAttr) {
	for i, attr := range attrs {
		if i == 0 {
			line.WriteString("  ")
		} else {
			line.WriteByte(' ')
		}
		switch attr.key {
		case "request_id", LogTraceIDKey, LogSpanIDKey:
			// Correlation IDs are for searching, not reading.
			line.WriteString(o.paint(ansiDim, clean(attr.key)+"="+formatValue(attr.value)))
		default:
			line.WriteString(o.paint(ansiDim, clean(attr.key)+"=") + formatValue(attr.value))
		}
	}
}

// formatValue renders a value as the text handler does: quoted when it holds
// a space, a quote, an equals sign or a control character.
func formatValue(value slog.Value) string {
	var text string
	switch value.Kind() {
	case slog.KindString:
		text = value.String()
	case slog.KindInt64:
		return strconv.FormatInt(value.Int64(), 10)
	case slog.KindUint64:
		return strconv.FormatUint(value.Uint64(), 10)
	case slog.KindFloat64:
		return strconv.FormatFloat(value.Float64(), 'g', -1, 64)
	case slog.KindBool:
		return strconv.FormatBool(value.Bool())
	case slog.KindDuration:
		return formatDuration(value.Duration())
	case slog.KindTime:
		return value.Time().Format("2006-01-02T15:04:05.000Z07:00")
	default:
		text = encodeLogValue(LogFormatText, value.Any())
	}
	if text == "" || strings.ContainsAny(text, " \"=") || hasControl(text) {
		return strconv.Quote(text)
	}
	return text
}

// formatDuration renders d with at most one decimal: 850µs, 4.1ms, 35ms,
// 1.25s.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return strconv.FormatInt(d.Microseconds(), 10) + "µs"
	case d < time.Second:
		return strings.TrimSuffix(strconv.FormatFloat(float64(d)/float64(time.Millisecond), 'f', 1, 64), ".0") + "ms"
	}
	return strings.TrimSuffix(strings.TrimSuffix(strconv.FormatFloat(d.Seconds(), 'f', 2, 64), "0"), ".0") + "s"
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return true
		}
	}
	return false
}

// clean returns s with control characters escaped (\n, \x1b), so that text
// from a request or a job can neither end the line nor drive the terminal.
func clean(s string) string {
	if !hasControl(s) {
		return s
	}
	var out strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			out.WriteString(`\n`)
		case r == '\r':
			out.WriteString(`\r`)
		case r == '\t':
			out.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			out.WriteString(`\x` + strconv.FormatInt(int64(r)+0x100, 16)[1:])
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}
