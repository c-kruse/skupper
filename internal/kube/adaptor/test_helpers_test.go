package adaptor

import "github.com/skupperproject/skupper/internal/routercontrol"

func testIntent() routercontrol.RouterIntent {
	return routercontrol.RouterIntent{
		SchemaVersion:     routercontrol.SchemaVersion,
		Target:            routercontrol.TargetIdentity{NamespaceUID: "n", SiteUID: "s", RouterGroup: "g"},
		Settings:          routercontrol.RouterSettings{Mode: routercontrol.RoutingModeInterior},
		ServiceConnectors: []routercontrol.ServiceConnector{{ID: "connector", RoutingKey: "orders", Protocol: routercontrol.ProtocolTCP, Target: routercontrol.TargetIdentity{NamespaceUID: "n", SiteUID: "s", RouterGroup: "g"}, TLS: routercontrol.TLSIntent{Mode: routercontrol.TLSModeDisabled}, Endpoints: []routercontrol.Endpoint{{ID: "pod", Host: "10.0.0.2", Port: 8080}}}},
	}
}
