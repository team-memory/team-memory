package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracer is the instrumentation scope for Team Memory spans.
var Tracer = otel.Tracer("github.com/team-memory/team-memory")

// SetupTracing installs a global tracer provider that exports over OTLP/HTTP
// when OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_TRACES_ENDPOINT is
// set (standard OpenTelemetry variables, including OTEL_TRACES_SAMPLER and
// OTEL_RESOURCE_ATTRIBUTES). Without an endpoint, spans are still created so
// trace ids reach logs and audit events, but nothing is exported. The W3C
// trace context propagator is always installed.
func SetupTracing(ctx context.Context, service, version string, logger *slog.Logger) (shutdown func(context.Context) error, err error) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(service), semconv.ServiceVersion(version)))
	if err != nil && !errors.Is(err, resource.ErrPartialResource) && !errors.Is(err, resource.ErrSchemaURLConflict) {
		return nil, err
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
		logger.Info("exporting traces over OTLP/HTTP")
	}
	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { logger.Warn("opentelemetry", "err", err) }))
	return tp.Shutdown, nil
}

// HTTPMiddleware starts a server span per request, continuing an incoming
// W3C traceparent. Span names use the normalized route.
func HTTPMiddleware(portalBase string, next http.Handler) http.Handler {
	prop := otel.GetTextMapPropagator()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		route := Route(r.URL.Path, portalBase)
		ctx, span := Tracer.Start(ctx, r.Method+" "+route, trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(semconv.HTTPRequestMethodKey.String(r.Method), semconv.HTTPRoute(route),
				attribute.String("url.path", truncate(r.URL.Path, 200))))
		defer span.End()
		rec := &codeRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))
		span.SetAttributes(semconv.HTTPResponseStatusCode(rec.code))
		if rec.code >= 500 {
			span.SetStatus(codes.Error, http.StatusText(rec.code))
		}
	})
}

// TraceID returns the current trace id, or "".
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// Transport wraps an HTTP client transport with client spans and trace
// context injection (the Ollama client).
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripper{base}
}

type roundTripper struct{ base http.RoundTripper }

func (t roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, span := Tracer.Start(r.Context(), r.Method+" "+r.URL.Host, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(semconv.HTTPRequestMethodKey.String(r.Method), semconv.ServerAddress(r.URL.Hostname())))
	defer span.End()
	r = r.Clone(ctx)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(r.Header))
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "request failed")
		return nil, err
	}
	span.SetAttributes(semconv.HTTPResponseStatusCode(resp.StatusCode))
	return resp, nil
}

// PGXTracer creates a client span per query. The statement is reduced to
// its sqlc name ("-- name: X") or first line; parameters are never
// recorded.
type PGXTracer struct{}

type spanKey struct{}

// TraceQueryStart implements pgx.QueryTracer.
func (PGXTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx // only as part of a traced request or job
	}
	name := queryName(data.SQL)
	ctx, span := Tracer.Start(ctx, "db "+name, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system", "postgresql"), attribute.String("db.operation.name", name)))
	return context.WithValue(ctx, spanKey{}, span)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (PGXTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(spanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil && !errors.Is(data.Err, pgx.ErrNoRows) {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, "query failed")
	}
	span.End()
}

func queryName(sql string) string {
	s := strings.TrimSpace(sql)
	if rest, ok := strings.CutPrefix(s, "-- name: "); ok {
		if f := strings.Fields(rest); len(f) > 0 {
			return f[0]
		}
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 60)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// MultiTracer runs several pgx tracers (the app pool's TxGuard and spans).
type MultiTracer []pgx.QueryTracer

// TraceQueryStart implements pgx.QueryTracer.
func (m MultiTracer) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	for _, t := range m {
		if t != nil {
			ctx = t.TraceQueryStart(ctx, c, d)
		}
	}
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (m MultiTracer) TraceQueryEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryEndData) {
	for i := len(m) - 1; i >= 0; i-- {
		if m[i] != nil {
			m[i].TraceQueryEnd(ctx, c, d)
		}
	}
}
