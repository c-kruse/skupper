package collector

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	opmetrics "github.com/skupperproject/skupper/cmd/network-observer/internal/collector/metrics"
	"github.com/skupperproject/skupper/pkg/vanflow"
)

func TestNetworkMetricLabels(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	for _, identity := range []struct{ network, observer string }{
		{"A", "one"}, {"A", "two"}, {"B", "three"},
	} {
		registerer := prometheus.WrapRegistererWith(prometheus.Labels{
			"network_id": identity.network, "observer_id": identity.observer,
		}, reg)
		m := register(registerer)
		flow := labelSet{SourceSiteID: "source", DestSiteID: "dest"}.asLabels()
		m.flowBytesSentCounter.With(flow).Add(7)
		m.internal.flowLatency.With(flow).Observe(0.5)
		flow["direction"] = "incoming"
		m.internal.legancyLatency.With(flow).Observe(500)
		m.internal.pendingFlows.WithLabelValues("tcp", "missing", "router").Set(3)
		topology := opmetrics.New(registerer)
		topology.Add(vanflow.SiteRecord{
			BaseRecord: vanflow.NewBase("site"), Name: ptrTo("name"), Version: ptrTo("2.0"),
		})
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 5 {
		t.Fatalf("expected flow, two histograms, internal and topology metrics, got %d", len(families))
	}
	for _, family := range families {
		if len(family.Metric) != 3 {
			t.Errorf("%s: expected separate series for all replicas/networks", family.GetName())
		}
		seen := map[string]string{}
		for _, metric := range family.Metric {
			values := map[string]string{}
			for _, label := range metric.Label {
				values[label.GetName()] = label.GetValue()
			}
			seen[values["observer_id"]] = values["network_id"]
		}
		if len(seen) != 3 || seen["one"] != "A" || seen["two"] != "A" || seen["three"] != "B" {
			t.Errorf("%s: incorrect identities: %v", family.GetName(), seen)
		}
	}
}
