package backend

import (
	"time"

	"github.com/cschleiden/go-workflows/internal/converter"
	"github.com/cschleiden/go-workflows/internal/logger"
	mi "github.com/cschleiden/go-workflows/internal/metrics"
	"github.com/cschleiden/go-workflows/log"
	"github.com/cschleiden/go-workflows/metrics"
	"go.opentelemetry.io/otel/trace"
)

type Options struct {
	Logger log.Logger

	Metrics metrics.Client

	TracerProvider trace.TracerProvider

	// Converter is the converter to use for serializing and deserializing inputs and results. If not explicitly set
	// converter.DefaultConverter is used.
	Converter converter.Converter

	StickyTimeout time.Duration

	// WorkflowLockTimeout determines how long a workflow task can be locked for. If the workflow task is not completed
	// by that timeframe, it's considered abandoned and another worker might pick it up.
	//
	// For long running workflow tasks, combine this with heartbearts.
	WorkflowLockTimeout time.Duration

	// ActivityLockTimeout determines how long an activity task can be locked for. If the activity task is not completed
	// by that timeframe, it's considered abandoned and another worker might pick it up
	ActivityLockTimeout time.Duration
}

var DefaultOptions Options = Options{
	StickyTimeout:       30 * time.Second,
	WorkflowLockTimeout: time.Minute,
	ActivityLockTimeout: time.Minute * 2,

	Logger:         logger.NewDefaultLogger(),
	Metrics:        mi.NewNoopMetricsClient(),
	TracerProvider: trace.NewNoopTracerProvider(),
	Converter:      converter.DefaultConverter,
}

type BackendOption func(*Options)

func WithStickyTimeout(timeout time.Duration) BackendOption {
	return func(o *Options) {
		o.StickyTimeout = timeout
	}
}

func WithLogger(logger log.Logger) BackendOption {
	return func(o *Options) {
		o.Logger = logger
	}
}

func WithMetrics(client metrics.Client) BackendOption {
	return func(o *Options) {
		o.Metrics = client
	}
}

func WithTracerProvider(tp trace.TracerProvider) BackendOption {
	return func(o *Options) {
		o.TracerProvider = tp
	}
}

func WithConverter(converter converter.Converter) BackendOption {
	return func(o *Options) {
		o.Converter = converter
	}
}

func ApplyOptions(opts ...BackendOption) Options {
	options := DefaultOptions

	for _, opt := range opts {
		opt(&options)
	}

	if options.Logger == nil {
		options.Logger = logger.NewDefaultLogger()
	}

	return options
}
