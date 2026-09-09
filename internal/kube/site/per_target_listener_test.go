package site

import (
	"log/slog"
	"testing"

	"github.com/skupperproject/skupper/internal/qdr"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	"gotest.tools/v3/assert"
)

func TestFindTargetsInNetwork(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		network  []skupperv2alpha1.SiteRecord
		expected []string
	}{
		{
			name:     "empty network returns empty results",
			prefix:   "myservice.",
			network:  []skupperv2alpha1.SiteRecord{},
			expected: []string{},
		},
		{
			name:   "no matching services returns empty results",
			prefix: "myservice.",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "notmatching.target1",
							Connectors: []string{"connector1"},
						},
					},
				},
			},
			expected: []string{},
		},
		{
			name:   "matching service with connectors returns target",
			prefix: "myservice.",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target1",
							Connectors: []string{"connector1"},
						},
					},
				},
			},
			expected: []string{"target1"},
		},
		{
			name:   "matching service without connectors returns nothing",
			prefix: "myservice.",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target1",
							Connectors: []string{},
						},
					},
				},
			},
			expected: []string{},
		},
		{
			name:   "2 matching targets 1 non matching.",
			prefix: "myservice.",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target1",
							Connectors: []string{"connector1"},
						},
						{
							RoutingKey: "myservice.target2",
							Connectors: []string{"connector2"},
						},
						{
							RoutingKey: "notmatching.target3",
							Connectors: []string{"connector3"},
						},
					},
				},
			},
			expected: []string{"target1", "target2"},
		},
		{
			name:   "multiple targets from multiple sites",
			prefix: "myservice.",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target1",
							Connectors: []string{"connector1"},
						},
					},
				},
				{
					Id:   "site2",
					Name: "site2",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target2",
							Connectors: []string{"connector2"},
						},
					},
				},
			},
			expected: []string{"target1", "target2"},
		},
		{
			name:   "empty prefix matches all services with connectors",
			prefix: "",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "service1.target1",
							Connectors: []string{"connector1"},
						},
						{
							RoutingKey: "service2.target2",
							Connectors: []string{"connector2"},
						},
					},
				},
			},
			expected: []string{"service1.target1", "service2.target2"},
		},
		{
			name:   "prefix with no dot separator",
			prefix: "myservice",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice-target1",
							Connectors: []string{"connector1"},
						},
						{
							RoutingKey: "myserviceother",
							Connectors: []string{"connector2"},
						},
					},
				},
			},
			expected: []string{"-target1", "other"},
		},
		{
			name:   "duplicate targets from different sites",
			prefix: "myservice.",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target1",
							Connectors: []string{"connector1"},
						},
					},
				},
				{
					Id:   "site2",
					Name: "site2",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target1",
							Connectors: []string{"connector2"},
						},
					},
				},
			},
			expected: []string{"target1", "target1"},
		},
		{
			name:   "mixed services with and without connectors",
			prefix: "myservice.",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:   "site1",
					Name: "site1",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target1",
							Connectors: []string{"connector1"},
						},
						{
							RoutingKey: "myservice.target2",
							Connectors: []string{},
						},
						{
							RoutingKey: "myservice.target3",
							Connectors: []string{"connector3"},
						},
					},
				},
			},
			expected: []string{"target1", "target3"},
		},
		{
			name:   "site with no services",
			prefix: "myservice.",
			network: []skupperv2alpha1.SiteRecord{
				{
					Id:       "site1",
					Name:     "site1",
					Services: []skupperv2alpha1.ServiceRecord{},
				},
				{
					Id:   "site2",
					Name: "site2",
					Services: []skupperv2alpha1.ServiceRecord{
						{
							RoutingKey: "myservice.target1",
							Connectors: []string{"connector1"},
						},
					},
				},
			},
			expected: []string{"target1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findTargetsInNetwork(tt.prefix, tt.network)

			if len(tt.expected) == 0 && len(result) == 0 {
				return
			}

			assert.DeepEqual(t, result, tt.expected)
		})
	}
}

func TestExtractTargetsReportsAddAndRemove(t *testing.T) {
	listener := &skupperv2alpha1.Listener{}
	listener.Name = "backend"
	listener.Spec.RoutingKey = "backend"
	listener.Spec.Port = 8080
	perTarget := newPerTargetListener(listener, slog.Default())
	mapping := qdr.RecoverPortMapping(nil)
	exposed := ExposedPorts{}
	context := NewMockBindingContext(nil)

	changed, err := perTarget.extractTargets([]string{"old"}, mapping, exposed, context)
	assert.NilError(t, err)
	assert.Assert(t, changed)

	changed, err = perTarget.extractTargets([]string{"new"}, mapping, exposed, context)
	assert.NilError(t, err)
	assert.Assert(t, changed)
	_, oldExists := perTarget.targets["old"]
	_, newExists := perTarget.targets["new"]
	assert.Assert(t, !oldExists)
	assert.Assert(t, newExists)

	changed, err = perTarget.extractTargets(nil, mapping, exposed, context)
	assert.NilError(t, err)
	assert.Assert(t, changed, "removing the final stale target must report a configuration change")
}

func TestAdaptorConfigDeduplicatesAndSortsPrefixes(t *testing.T) {
	bindings := &ExtendedBindings{perTargetListeners: map[string]*PerTargetListener{
		"z":         newPerTargetListener(&skupperv2alpha1.Listener{Spec: skupperv2alpha1.ListenerSpec{RoutingKey: "zebra"}}, slog.Default()),
		"a":         newPerTargetListener(&skupperv2alpha1.Listener{Spec: skupperv2alpha1.ListenerSpec{RoutingKey: "alpha"}}, slog.Default()),
		"duplicate": newPerTargetListener(&skupperv2alpha1.Listener{Spec: skupperv2alpha1.ListenerSpec{RoutingKey: "alpha"}}, slog.Default()),
	}}

	assert.DeepEqual(t, bindings.adaptorConfig().AddressPrefixes, []qdr.AddressPrefix{
		{Prefix: "alpha."},
		{Prefix: "zebra."},
	})
}
