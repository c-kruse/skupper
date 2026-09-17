package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/VictoriaMetrics/metricsql"
)

func TestScopePrometheusQuery(t *testing.T) {
	for _, query := range []string{
		`skupper_sent_bytes_total`,
		`{__name__="skupper_sent_bytes_total",protocol=~"tcp|http"}`,
		`skupper_sent_bytes_total{network_id="A"}`,
		`sum(rate(skupper_sent_bytes_total[2m])) / sum(rate(skupper_received_bytes_total[2m]))`,
		`max_over_time((skupper_connections_opened_total-skupper_connections_closed_total)[5m:1m])`,
		`histogram_quantile(0.95, sum by(le)(rate(legacy_flow_latency_microseconds_bucket[2m])))`,
		`skupper_site_info offset 5m or skupper_site_links_total @ 1234`,
	} {
		t.Run(query, func(t *testing.T) {
			result, err := scopePrometheusQuery(query, "A")
			if err != nil {
				t.Fatal(err)
			}
			expr, err := metricsql.Parse(result)
			if err != nil {
				t.Fatal(err)
			}
			selectors := 0
			metricsql.VisitAll(expr, func(node metricsql.Expr) {
				if selector, ok := node.(*metricsql.MetricExpr); ok {
					selectors++
					networks := 0
					if len(selector.LabelFilterss) != 1 {
						t.Fatalf("unexpected filter groups: %v", selector.LabelFilterss)
					}
					for _, matcher := range selector.LabelFilterss[0] {
						if matcher.Label == "network_id" {
							networks++
							if matcher.IsNegative || matcher.IsRegexp || matcher.Value != "A" {
								t.Errorf("unscoped selector: %s", selector.AppendString(nil))
							}
						}
					}
					if networks != 1 {
						t.Errorf("expected one network matcher, got %d", networks)
					}
				}
			})
			if selectors == 0 {
				t.Fatal("no selectors checked")
			}
			again, err := scopePrometheusQuery(result, "A")
			if err != nil || again != result {
				t.Fatalf("rewrite is not idempotent: %q, %v", again, err)
			}
		})
	}
	for _, query := range []string{
		`up`, `kube_pod_info`, `skupper_not_authorized`,
		`{__name__=~"skupper_.*"}`, `{__name__=~"skupper_sent_bytes_total"}`,
		`{__name__!="up",site_id="site-a"}`, `{site_id="site-a"}`,
		`skupper_sent_bytes_total{network_id="B"}`,
		`skupper_sent_bytes_total{network_id=~"A|B"}`,
		`skupper_sent_bytes_total{network_id!="B"}`,
		`skupper_sent_bytes_total{network_id="A",network_id="B"}`,
		`sum(rate(skupper_sent_bytes_total[2m])) + sum(up)`,
		`up or skupper_sent_bytes_total`,
		`skupper_sent_bytes_total{network_id="A" or network_id="B"}`,
		`{__name__="skupper_sent_bytes_total" or __name__="up"}`,
		`skupper_sent_bytes_total @ scalar(up)`,
		`WITH (hidden = up) sum(hidden)`,
	} {
		t.Run("reject "+query, func(t *testing.T) {
			if _, err := scopePrometheusQuery(query, "A"); !errors.Is(err, errForbiddenPrometheusQuery) {
				t.Fatalf("expected forbidden, got %v", err)
			}
		})
	}
	if _, err := scopePrometheusQuery(`sum(`, "A"); err == nil || errors.Is(err, errForbiddenPrometheusQuery) {
		t.Fatalf("expected parse error, got %v", err)
	}
	// Network IDs are data, not PromQL syntax.
	if got, err := scopePrometheusQuery(`skupper_site_info`, "a\"\\b"); err != nil || got != `skupper_site_info{network_id="a\"\\b"}` {
		t.Fatalf("unexpected escaped network ID: %q, %v", got, err)
	}
	if _, err := scopePrometheusQuery(`info(skupper_site_info)`, "A"); err == nil {
		t.Fatal("implicit info lookup must not be allowed")
	}
}

func TestPrometheusProxyRequestValidation(t *testing.T) {
	forwarded := make(chan url.Values, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		forwarded <- r.Form
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	target, err := parsePrometheusAPI(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := handleProxyPrometheusAPI("/prom", target, "A")
	const valid = "query=skupper_sent_bytes_total"
	for _, tc := range []struct {
		name, method, params, body, contentType string
		status                                  int
	}{
		{"get", "GET", valid + "&time=123", "", "", 200},
		{"post", "POST", "time=123", valid, "application/x-www-form-urlencoded", 200},
		{"post URL query", "POST", valid + "&time=123", "", "application/x-www-form-urlencoded", 200},
		{"missing", "GET", "", "", "", 400},
		{"invalid syntax", "GET", "query=sum%28", "", "", 400},
		{"unknown metric", "GET", "query=up", "", "", 403},
		{"other network", "GET", "query=" + url.QueryEscape(`skupper_sent_bytes_total{network_id="B"}`), "", "", 403},
		{"duplicate URL", "GET", valid + "&query=up", "", "", 400},
		{"duplicate sources", "POST", "query=up", valid, "application/x-www-form-urlencoded", 400},
		{"duplicate body", "POST", "", valid + "&query=up", "application/x-www-form-urlencoded", 400},
		{"invalid encoding", "GET", valid + "&bad=%ZZ", "", "", 400},
		{"GET body", "GET", valid, "query=up", "application/x-www-form-urlencoded", 400},
		{"unsupported method", "PUT", valid, "", "", 405},
		{"unsupported body", "POST", valid, "{}", "application/json", 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "/prom/query?"+tc.params, strings.NewReader(tc.body))
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			select {
			case form := <-forwarded:
				if tc.status != 200 {
					t.Fatal("rejected query reached upstream")
				}
				if len(form["query"]) != 1 || form.Get("query") != `skupper_sent_bytes_total{network_id="A"}` || form.Get("time") != "123" {
					t.Fatalf("unexpected upstream form: %v", form)
				}
			default:
				if tc.status == 200 {
					t.Fatal("query not forwarded")
				}
			}
		})
	}
}
