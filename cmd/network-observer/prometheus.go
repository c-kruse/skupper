package main

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/VictoriaMetrics/metricsql"
)

var allowedPrometheusMetrics = map[string]bool{
	"skupper_connections_opened_total":                true,
	"skupper_connections_closed_total":                true,
	"skupper_sent_bytes_total":                        true,
	"skupper_received_bytes_total":                    true,
	"skupper_requests_total":                          true,
	"skupper_site_info":                               true,
	"skupper_routers_total":                           true,
	"skupper_site_listeners_total":                    true,
	"skupper_site_connectors_total":                   true,
	"skupper_site_links_total":                        true,
	"skupper_site_link_errors_total":                  true,
	"skupper_internal_latency_seconds_bucket":         true,
	"skupper_internal_latency_seconds_sum":            true,
	"skupper_internal_latency_seconds_count":          true,
	"skupper_internal_collector_job_seconds_bucket":   true,
	"skupper_internal_collector_job_seconds_sum":      true,
	"skupper_internal_collector_job_seconds_count":    true,
	"skupper_internal_flow_processing_seconds_bucket": true,
	"skupper_internal_flow_processing_seconds_sum":    true,
	"skupper_internal_flow_processing_seconds_count":  true,
	"skupper_internal_queue_utilization_percentage":   true,
	"skupper_internal_pending_flows":                  true,
	"legacy_flow_latency_microseconds_bucket":         true,
	"legacy_flow_latency_microseconds_sum":            true,
	"legacy_flow_latency_microseconds_count":          true,
}

var errForbiddenPrometheusQuery = errors.New("forbidden Prometheus query")

func scopePrometheusQuery(query, networkID string) (string, error) {
	expr, err := metricsql.Parse(query)
	if err != nil {
		return "", err
	}
	var policyErr error
	metricsql.VisitAll(expr, func(node metricsql.Expr) {
		if policyErr != nil {
			return
		}
		// info() can read series implicitly, outside the explicit selectors.
		if call, ok := node.(*metricsql.FuncExpr); ok && call.Name == "info" {
			policyErr = fmt.Errorf("%w: info is not supported", errForbiddenPrometheusQuery)
			return
		}
		selector, ok := node.(*metricsql.MetricExpr)
		if !ok {
			return
		}
		// Selector-local OR groups are a MetricsQL extension, not PromQL.
		if len(selector.LabelFilterss) != 1 {
			policyErr = fmt.Errorf("%w: selector must have one filter group", errForbiddenPrometheusQuery)
			return
		}
		name := ""
		for _, matcher := range selector.LabelFilterss[0] {
			switch matcher.Label {
			case "__name__":
				if matcher.IsNegative || matcher.IsRegexp || !allowedPrometheusMetrics[matcher.Value] {
					policyErr = fmt.Errorf("%w: metric is not allow-listed", errForbiddenPrometheusQuery)
					return
				}
				name = matcher.Value
			case "network_id":
				if matcher.IsNegative || matcher.IsRegexp || matcher.Value != networkID {
					policyErr = fmt.Errorf("%w: network_id must equal the observer network", errForbiddenPrometheusQuery)
					return
				}
			}
		}
		if name == "" {
			policyErr = fmt.Errorf("%w: an exact metric name is required", errForbiddenPrometheusQuery)
			return
		}
		// Replace existing equal matchers with one authoritative matcher.
		matchers := selector.LabelFilterss[0][:0]
		for _, matcher := range selector.LabelFilterss[0] {
			if matcher.Label != "network_id" {
				matchers = append(matchers, matcher)
			}
		}
		selector.LabelFilterss[0] = append(matchers, metricsql.LabelFilter{Label: "network_id", Value: networkID})
	})
	if policyErr != nil {
		return "", policyErr
	}
	return string(expr.AppendString(nil)), nil
}

func scopePrometheusRequest(w http.ResponseWriter, r *http.Request, networkID string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if r.Method == http.MethodPost {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			http.Error(w, "expected form-encoded query", http.StatusUnsupportedMediaType)
			return false
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	} else if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "GET query must not have a body", http.StatusBadRequest)
		return false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid query parameters", http.StatusBadRequest)
		return false
	}
	queries := r.Form["query"]
	if len(queries) != 1 || strings.TrimSpace(queries[0]) == "" {
		http.Error(w, "exactly one query is required", http.StatusBadRequest)
		return false
	}
	query, err := scopePrometheusQuery(queries[0], networkID)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errForbiddenPrometheusQuery) {
			status = http.StatusForbidden
		}
		http.Error(w, err.Error(), status)
		return false
	}
	// Normalize to one parameter source so upstream cannot read an unvalidated
	// query from a different location or apply different precedence rules.
	r.Form.Set("query", query)
	if r.Method == http.MethodPost {
		body := r.Form.Encode()
		r.URL.RawQuery = ""
		r.Body = io.NopCloser(strings.NewReader(body))
		r.ContentLength = int64(len(body))
		r.TransferEncoding = nil
	} else {
		r.URL.RawQuery = r.Form.Encode()
	}
	return true
}
