// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Traces over OpenTelemetry: a server span per request, a span per chat
// answer, per round with the LLM and per tool call, and client spans for
// the calls to the LLM gateway and to ferund, which carry the trace on
// (traceparent). The exporter is OTLP over HTTP and takes its address
// from the environment as the SDK reads it: FeRun's nodes set
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT and OTEL_SERVICE_NAME in every pod.
// Without an endpoint no span leaves the process.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// tracer makes FeChat's own spans; a no-op until setupTracing installs a
// provider.
var tracer trace.Tracer = otel.Tracer("fechat-backend")

// setupTracing installs the tracer provider and the W3C propagator. It
// returns the function that flushes the spans at shutdown.
func setupTracing(ctx context.Context) func() {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))
	if os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		log.Printf("no OTLP endpoint: no traces")
		return func() {}
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		log.Printf("traces off: %v", err)
		return func() {}
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(serviceName())))
	if err != nil {
		res = resource.Default()
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	tracer = tp.Tracer("fechat-backend")
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			log.Printf("traces: %v", err)
		}
	}
}

// serviceName is OTEL_SERVICE_NAME, else "fechat-backend".
func serviceName() string {
	if n := os.Getenv("OTEL_SERVICE_NAME"); n != "" {
		return n
	}
	return "fechat-backend"
}

// tracedServer wraps the routes in a server span each, named after the
// route's pattern; the health check, asked every few seconds, makes
// none.
func tracedServer(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "fechat-backend",
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/api/v1/healthz" }),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			if r.Pattern != "" {
				return r.Pattern
			}
			return r.Method
		}))
}

// tracedTransport makes a client span of every request through rt and
// sends the trace along.
func tracedTransport(rt http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(rt)
}
