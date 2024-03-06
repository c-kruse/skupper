package status

import (
	"github.com/c-kruse/vanflow"
	"github.com/skupperproject/skupper/pkg/network"
)

func asAddressInfo(address string, connectors []*vanflow.ConnectorRecord, listeners []*vanflow.ListenerRecord) network.AddressInfo {
	var addressInfo network.AddressInfo
	addressInfo.Name = address
	for _, connector := range connectors {
		if connector.Protocol != nil {
			addressInfo.Protocol = *connector.Protocol
			break
		}
	}
	addressInfo.ConnectorCount = len(connectors)
	addressInfo.ListenerCount = len(listeners)
	return addressInfo
}

func asLinkInfo(link vanflow.LinkRecord) network.LinkInfo {
	return network.LinkInfo{
		Mode:      dref(link.Mode),
		Name:      dref(link.Name),
		LinkCost:  dref(link.LinkCost),
		Direction: dref(link.Direction),
	}
}

func asConnectorInfo(connector vanflow.ConnectorRecord) network.ConnectorInfo {
	return network.ConnectorInfo{
		DestHost: dref(connector.DestHost),
		DestPort: dref(connector.DestPort),
		Address:  dref(connector.Address),
	}
}

func asListenerInfo(listener vanflow.ListenerRecord) network.ListenerInfo {
	return network.ListenerInfo{
		//TODO(ck) Name not in spec? Name: dref(listener.Name),
		DestHost: dref(listener.DestHost),
		DestPort: dref(listener.DestPort),
		Protocol: dref(listener.Protocol),
		Address:  dref(listener.Address),
	}
}

func asSiteInfo(site vanflow.SiteRecord) network.SiteInfo {
	return network.SiteInfo{
		Identity:  site.ID,
		Name:      dref(site.Name),
		Namespace: dref(site.Namespace),
		Platform:  dref(site.Platform),
		Version:   dref(site.Version),
		Policy:    dref(site.Policy),
	}
}

func asRouterInfo(router vanflow.RouterRecord) network.RouterInfo {
	return network.RouterInfo{
		Name:         dref(router.Name),
		Namespace:    dref(router.Namespace),
		Mode:         dref(router.Mode),
		ImageName:    dref(router.ImageName),
		ImageVersion: dref(router.ImageVersion),
		Hostname:     dref(router.Hostname),
	}
}

func dref[T any, R *T](ptr R) T {
	var out T
	if ptr != nil {
		out = *ptr
	}
	return out
}
