package api

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "typstpdf_http_request_duration_seconds",
	Help:    "HTTP request latency by route pattern and status.",
	Buckets: prometheus.DefBuckets,
}, []string{"method", "route", "status"})
