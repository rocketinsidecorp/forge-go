package telemetry

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestParseTraceparentAcceptsValidValues(t *testing.T) {
	sc, err := ParseTraceparent(validTraceparent)
	if err != nil {
		t.Fatal(err)
	}
	if sc.TraceID.String() != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.SpanID.String() != "00f067aa0ba902b7" || !sc.IsSampled() {
		t.Fatalf("parsed %+v", sc)
	}
	if got := FormatTraceparent(sc); got != validTraceparent {
		t.Fatalf("FormatTraceparent = %q", got)
	}

	notSampled, err := ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	if err != nil || notSampled.IsSampled() {
		t.Fatalf("unsampled: %+v %v", notSampled, err)
	}
	// Unknown flag bits are accepted but not propagated.
	unknownFlags, err := ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-ff")
	if err != nil || unknownFlags.Flags != FlagSampled {
		t.Fatalf("unknown flags: %+v %v", unknownFlags, err)
	}
	// A future version may append fields after a dash.
	future, err := ParseTraceparent("cc-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-what-the-future-holds")
	if err != nil || future.TraceID != sc.TraceID {
		t.Fatalf("future version: %+v %v", future, err)
	}
}

func TestParseTraceparentRejectsMalformedValues(t *testing.T) {
	for name, value := range map[string]string{
		"empty":               "",
		"short":               validTraceparent[:54],
		"version 00 too long": validTraceparent + "-extra",
		"version ff":          "ff" + validTraceparent[2:],
		"uppercase trace":     "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",
		"uppercase version":   "0A" + validTraceparent[2:],
		"zero trace":          "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"zero span":           "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"bad separator":       "00_4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"non hex flags":       "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-0g",
		"future no dash":      "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01x",
		"whitespace":          " " + validTraceparent[1:],
		"non ascii":           "00-4bf92f3577b34da6a3ce929d0e0e47é-00f067aa0ba902b7-01",
	} {
		t.Run(name, func(t *testing.T) {
			if sc, err := ParseTraceparent(value); err == nil {
				t.Fatalf("ParseTraceparent(%q) = %+v, want error", value, sc)
			}
		})
	}
}

func TestNormalizeTracestate(t *testing.T) {
	valid := map[string]string{
		"":                                  "",
		"congo=t61rcWkgMzE":                 "congo=t61rcWkgMzE",
		" rojo=00f067aa0ba902b7 ,, congo=t": "rojo=00f067aa0ba902b7,congo=t",
		"tenant@vendor=value":               "tenant@vendor=value",
		"1tenant@v=x":                       "1tenant@v=x",
		"a=b c":                             "a=b c",
		"a=b ":                              "a=b", // optional whitespace around members

	}
	for input, want := range valid {
		got, ok := NormalizeTracestate(input)
		if !ok || got != want {
			t.Errorf("NormalizeTracestate(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
	tooMany := make([]string, 33)
	for i := range tooMany {
		tooMany[i] = "k" + strings.Repeat("a", i) + "=v"
	}
	invalid := []string{
		"novalue",
		"UPPER=x",
		"a=b,a=c",
		"a=",
		"a=b=c",
		"a=\x01",
		"@vendor=x",
		"tenant@=x",
		"tenant@toolongsystemidxx=x",
		"a=" + strings.Repeat("v", 257),
		strings.Join(tooMany, ","),
		strings.Repeat("abcdefgh=12345678,", 40),
	}
	for _, input := range invalid {
		if got, ok := NormalizeTracestate(input); ok {
			t.Errorf("NormalizeTracestate(%q) = %q, want rejection", input, got)
		}
	}
}

func TestExtractAndInject(t *testing.T) {
	header := http.Header{}
	header.Set(TraceparentHeader, validTraceparent)
	header.Add(TracestateHeader, "rojo=1")
	header.Add(TracestateHeader, "congo=2")
	ctx := Extract(context.Background(), header)
	sc := SpanContextFromContext(ctx)
	if !sc.IsValid() || !sc.Remote || sc.TraceState != "rojo=1,congo=2" {
		t.Fatalf("extracted %+v", sc)
	}

	out := http.Header{}
	out.Set(TracestateHeader, "stale=1")
	Inject(ctx, out)
	if out.Get(TraceparentHeader) != validTraceparent || out.Get(TracestateHeader) != "rojo=1,congo=2" {
		t.Fatalf("injected %v", out)
	}

	Inject(context.Background(), out)
	if out.Get(TraceparentHeader) != validTraceparent || out.Get(TracestateHeader) != "rojo=1,congo=2" {
		t.Fatalf("Inject without a span must leave existing headers alone: %v", out)
	}
}

func TestExtractIgnoresInvalidInput(t *testing.T) {
	repeated := http.Header{}
	repeated.Add(TraceparentHeader, validTraceparent)
	repeated.Add(TraceparentHeader, validTraceparent)
	if sc := SpanContextFromContext(Extract(context.Background(), repeated)); sc.IsValid() {
		t.Fatal("repeated traceparent headers must be ignored")
	}
	malformed := http.Header{}
	malformed.Set(TraceparentHeader, "garbage")
	if sc := SpanContextFromContext(Extract(context.Background(), malformed)); sc.IsValid() {
		t.Fatal("malformed traceparent must be ignored")
	}
	badState := http.Header{}
	badState.Set(TraceparentHeader, validTraceparent)
	badState.Set(TracestateHeader, "BAD")
	sc := SpanContextFromContext(Extract(context.Background(), badState))
	if !sc.IsValid() || sc.TraceState != "" {
		t.Fatalf("an invalid tracestate is dropped while traceparent is kept: %+v", sc)
	}
}

func FuzzParseTraceparent(f *testing.F) {
	for _, seed := range []string{
		validTraceparent,
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00",
		"cc-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-0000000000000000-00",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		sc, err := ParseTraceparent(value)
		if err != nil {
			if sc != (SpanContext{}) {
				t.Fatalf("error with non-zero result %+v", sc)
			}
			return
		}
		if !sc.IsValid() || sc.Flags&^FlagSampled != 0 {
			t.Fatalf("accepted %q as invalid span context %+v", value, sc)
		}
		formatted := FormatTraceparent(sc)
		again, err := ParseTraceparent(formatted)
		if err != nil || again != sc {
			t.Fatalf("round trip of %q: %q -> %+v, %v", value, formatted, again, err)
		}
		if value[:2] == "00" && formatted[:53] != value[:53] {
			t.Fatalf("version 00 identifiers changed: %q -> %q", value, formatted)
		}
	})
}

func FuzzNormalizeTracestate(f *testing.F) {
	for _, seed := range []string{"", "a=b", "rojo=00f067aa0ba902b7,congo=t61rcWkgMzE", "t@v=x, ,a=1", "a=b,a=c"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		normalized, ok := NormalizeTracestate(value)
		if !ok {
			return
		}
		if len(normalized) > maxTracestateLength {
			t.Fatalf("normalized tracestate too long: %d", len(normalized))
		}
		again, ok := NormalizeTracestate(normalized)
		if !ok || again != normalized {
			t.Fatalf("normalization is not idempotent: %q -> %q -> %q", value, normalized, again)
		}
	})
}
