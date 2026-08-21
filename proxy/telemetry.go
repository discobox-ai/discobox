package proxy

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/discobox-ai/discobox/proxy"

func proxyTracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

func recordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func clientAttrs(client clientIdentity) []attribute.KeyValue {
	attrs := []attribute.KeyValue{}
	if client.ID != "" {
		attrs = append(attrs, attribute.String("proxy.client.id", client.ID))
	}
	if client.Serial != "" {
		attrs = append(attrs, attribute.String("proxy.client.cert_serial", client.Serial))
	}
	return attrs
}
