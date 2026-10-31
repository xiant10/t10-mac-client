// Package telemetry gives the client's work two IDs and carries them on a
// context.Context, so log lines and spans can be tied back together: a session
// (this connection to the room, named by the LiveKit participant SID the agent
// also sees) and a turn (one input and the reply to it, one trace across every
// process, so a turn_id is a trace ID).
//
// Mirrors (and is kept in sync by hand with) the agent's own
// internal/telemetry — unreachable here across module boundaries, so
// duplicated, the same as internal/audio's WAV codec and internal/room's
// token minting. See that package's doc comment for the full design
// rationale; this copy is trimmed to what the client actually needs. Here a
// trace either starts with StartTrace (connecting, a text or voice message) or
// is joined with ContextWithTraceparent (a live turn, which only the agent's
// VAD can start).
package telemetry

import (
	"context"
	"crypto/rand"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName scopes every span/metric this package produces.
const instrumentationName = "t10-client"

// traceContext is explicit rather than the otel global so propagation works
// whether or not SetupOTel ran.
var traceContext = propagation.TraceContext{}

// tracer and meter are the global otel.Tracer/otel.Meter handles: the
// go.opentelemetry.io/otel global package returns proxies that transparently
// start delegating to the real implementation the moment SetupOTel calls
// otel.SetTracerProvider/SetMeterProvider, so obtaining them once here (long
// before SetupOTel may or may not ever run) is the intended usage.
var (
	tracer = otel.Tracer(instrumentationName)
	meter  = otel.Meter(instrumentationName)

	stageDuration = mustFloat64Histogram(meter, "t10.stage.duration",
		"Duration of one client-side stage (connect, publish, playback).", "ms")
	stageCalls = mustInt64Counter(meter, "t10.stage.calls",
		"Count of completed stage calls, tagged by stage name and whether it errored.")
)

func mustFloat64Histogram(m metric.Meter, name, desc, unit string) metric.Float64Histogram {
	h, err := m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit(unit))
	if err != nil {
		panic(err) // only fails on a malformed instrument name/description — a programming bug
	}
	return h
}

func mustInt64Counter(m metric.Meter, name, desc string) metric.Int64Counter {
	c, err := m.Int64Counter(name, metric.WithDescription(desc))
	if err != nil {
		panic(err)
	}
	return c
}

type sessionKey struct{}

// WithSessionID stamps ctx with the session it belongs to.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, id)
}

// SessionID returns the ID WithSessionID stamped on ctx, or "".
func SessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}

func sessionAttr(ctx context.Context) []attribute.KeyValue {
	if id := SessionID(ctx); id != "" {
		return []attribute.KeyValue{attribute.String("session.id", id)}
	}
	return nil
}

// StartTrace opens the root span of a new trace, for something the client
// starts: connecting, or sending a message. Without an SDK the root has no IDs,
// so one is minted to keep TurnID and Traceparent working. Call the returned
// func once it is done.
func StartTrace(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(err error)) {
	ctx, span := tracer.Start(ctx, name, trace.WithNewRoot(), trace.WithAttributes(attrs...), trace.WithAttributes(sessionAttr(ctx)...))
	if !span.SpanContext().IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
			TraceID:    newTraceID(),
			SpanID:     newSpanID(),
			TraceFlags: trace.FlagsSampled,
		}))
	}
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// StartStage starts a child span for one stage of work within a trace —
// connecting, publishing the mic track, time-to-first-agent-audio — and
// starts timing it. Call the returned end func exactly once, with the error
// the stage finished with (nil on success): it records the error on the
// span (if any), closes it, and records the stage's duration and outcome as
// OTel metrics tagged by name, so a single call site drives both traces and
// metrics.
func StartStage(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(err error)) {
	ctx, span := tracer.Start(ctx, name, trace.WithAttributes(attrs...), trace.WithAttributes(sessionAttr(ctx)...))
	start := time.Now()
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()

		metricAttrs := metric.WithAttributes(
			attribute.String("stage", name),
			attribute.Bool("error", err != nil),
		)
		stageDuration.Record(ctx, float64(time.Since(start).Microseconds())/1000, metricAttrs)
		stageCalls.Add(ctx, 1, metricAttrs)
	}
}

// TurnID returns the ID of the trace on ctx, or "" outside one.
func TurnID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// Traceparent renders the span on ctx as a W3C traceparent, or "" outside a trace.
func Traceparent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	traceContext.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// ContextWithTraceparent makes traceparent the remote parent of spans started
// under the result. ctx is returned unchanged if traceparent is empty or malformed.
func ContextWithTraceparent(ctx context.Context, traceparent string) context.Context {
	return traceContext.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

func newTraceID() (id trace.TraceID) {
	_, _ = rand.Read(id[:])
	return id
}

func newSpanID() (id trace.SpanID) {
	_, _ = rand.Read(id[:])
	return id
}

// NewLogHandler wraps next so that any record logged through a context
// carrying a session or a trace gets session_id and turn_id attributes
// automatically. This is what gets the IDs into "logs along the way" without
// every log call site needing to know about them — they just need to use the
// *Context slog variants (InfoContext, ErrorContext, ...). Put it outside
// NewOTelLogHandler, so the OTel branch sees the attributes too.
func NewLogHandler(next slog.Handler) slog.Handler {
	return &logHandler{next: next}
}

type logHandler struct{ next slog.Handler }

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := SessionID(ctx); id != "" {
		r.AddAttrs(slog.String("session_id", id))
	}
	if id := TurnID(ctx); id != "" {
		r.AddAttrs(slog.String("turn_id", id))
	}
	return h.next.Handle(ctx, r)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{next: h.next.WithAttrs(attrs)}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{next: h.next.WithGroup(name)}
}

// NewOTelLogHandler returns a slog.Handler that bridges log records into the
// OTel log pipeline (via otelslog) *and* forwards them to next. This gives
// us both: logs exported via OTLP to whatever backend the collector routes
// them to, and the same logs written locally (to stderr/a file) by next.
// If no LoggerProvider has been set (SetupOTel was not called), the bridge
// half is a no-op — the same graceful-degradation contract as traces/metrics.
func NewOTelLogHandler(next slog.Handler) slog.Handler {
	bridge := otelslog.NewHandler(instrumentationName)
	return &fanoutHandler{local: next, otel: bridge}
}

type fanoutHandler struct {
	local slog.Handler
	otel  slog.Handler
}

func (f *fanoutHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return f.local.Enabled(ctx, level) || f.otel.Enabled(ctx, level)
}

func (f *fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	_ = f.otel.Handle(ctx, r)
	return f.local.Handle(ctx, r)
}

func (f *fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &fanoutHandler{local: f.local.WithAttrs(attrs), otel: f.otel.WithAttrs(attrs)}
}

func (f *fanoutHandler) WithGroup(name string) slog.Handler {
	return &fanoutHandler{local: f.local.WithGroup(name), otel: f.otel.WithGroup(name)}
}
