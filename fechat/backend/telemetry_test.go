// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestLLMSpan: a round with the LLM is a span with the model, the finish
// reason and the tokens of the stream's last chunk, and the gateway gets
// the trace (traceparent) of the client span below it.
func TestLLMSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	tracer = tp.Tracer("test")
	t.Cleanup(func() { tracer = otel.Tracer("fechat-backend") })

	var traceparent string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("Traceparent")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"model":"kimi-k3","choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer llm.Close()

	res := callLLM(context.Background(), config{BaseURL: llm.URL, Model: "kimi-k3"}, []Message{{Role: "user", Content: "x"}}, nil)
	for range res.Deltas {
	}
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	tp.ForceFlush(context.Background())

	var chat sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "chat kimi-k3" {
			chat = s
		}
	}
	if chat == nil {
		t.Fatalf("no span chat kimi-k3 among %d", len(rec.Ended()))
	}
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range chat.Attributes() {
		attrs[kv.Key] = kv.Value
	}
	if got := attrs["gen_ai.usage.input_tokens"].AsInt64(); got != 12 {
		t.Errorf("input tokens %d, want 12", got)
	}
	if got := attrs["gen_ai.usage.output_tokens"].AsInt64(); got != 3 {
		t.Errorf("output tokens %d, want 3", got)
	}
	if got := attrs["gen_ai.response.finish_reasons"].AsStringSlice(); len(got) != 1 || got[0] != "stop" {
		t.Errorf("finish reasons %v, want [stop]", got)
	}
	want := chat.SpanContext().TraceID().String()
	if len(traceparent) < 35 || traceparent[3:35] != want {
		t.Errorf("traceparent %q, want trace %s", traceparent, want)
	}
}
