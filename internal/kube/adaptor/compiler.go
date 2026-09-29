package adaptor

import (
	"crypto/sha256"
	"fmt"
	"strconv"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
	"github.com/skupperproject/skupper/internal/version"
)

const ownedNamePrefix = "skupper.v2."

type CredentialRealization struct {
	Profile       qdr.SslProfile
	RealizationID string
}

type CompiledIntent struct {
	Config        qdr.RouterConfig
	ResourceNames map[routercontrol.ResourceID][]string
	RealizationID string
	CredentialIDs map[routercontrol.ResourceID]string
}

func ownedName(kind string, id routercontrol.ResourceID) string {
	sum := sha256.Sum256([]byte(id))
	return fmt.Sprintf("%s%s.%x", ownedNamePrefix, kind, sum[:10])
}

// CompileIntent translates domain intent into local QDR entities. Bootstrap
// listeners are always present and are not controller-owned traffic entities.
func CompileIntent(intent routercontrol.RouterIntent, credentials map[routercontrol.ResourceID]CredentialRealization) (CompiledIntent, error) {
	if err := ValidateIntent(intent); err != nil {
		return CompiledIntent{}, err
	}
	edge := intent.Settings.Mode == routercontrol.RoutingModeEdge
	config := qdr.InitialConfig(intent.Target.RouterGroup+"-${HOSTNAME}", intent.Target.SiteUID, version.Version, edge, 3)
	config.AddAddress(qdr.Address{Prefix: "mc", Distribution: "multicast"})
	config.AddHealthAndMetricsListener(9090)
	config.AddListener(qdr.Listener{Name: "amqp", Host: "localhost", Port: 5672})
	result := CompiledIntent{Config: config, ResourceNames: map[routercontrol.ResourceID][]string{}, CredentialIDs: map[routercontrol.ResourceID]string{}}

	credentialProfile := func(owner routercontrol.ResourceID, tls routercontrol.TLSIntent) (string, error) {
		if tls.Mode == "" || tls.Mode == routercontrol.TLSModeDisabled {
			return "", nil
		}
		realization, found := credentials[tls.CredentialBinding]
		if !found {
			return "", fmt.Errorf("credential %q for resource %q is unavailable", tls.CredentialBinding, owner)
		}
		profile := realization.Profile
		profile.Name = ownedName("tls", tls.CredentialBinding)
		result.Config.AddSslProfile(profile)
		result.CredentialIDs[tls.CredentialBinding] = realization.RealizationID
		return profile.Name, nil
	}
	for _, resource := range intent.RouterConnections {
		profile, err := credentialProfile(resource.ID, resource.TLS)
		if err != nil {
			return CompiledIntent{}, err
		}
		name := ownedName("connection", resource.ID)
		result.Config.AddConnector(qdr.Connector{Name: name, Host: resource.Host, Port: strconv.Itoa(int(resource.Port)), Role: qdr.Role(resource.Role), Cost: int32(resource.Cost), SslProfile: profile, VerifyHostname: resource.TLS.VerifyHostname})
		result.ResourceNames[resource.ID] = []string{name}
	}
	for _, resource := range intent.RouterListeners {
		profile, err := credentialProfile(resource.ID, resource.TLS)
		if err != nil {
			return CompiledIntent{}, err
		}
		name := ownedName("router-listener", resource.ID)
		result.Config.AddListener(qdr.Listener{Name: name, Host: resource.Host, Port: int32(resource.Port), Role: qdr.Role(resource.Role), SslProfile: profile, AuthenticatePeer: resource.TLS.Mode == routercontrol.TLSModeMutual})
		result.ResourceNames[resource.ID] = []string{name}
	}
	for _, resource := range intent.ServiceListeners {
		if resource.Protocol != routercontrol.ProtocolTCP {
			return CompiledIntent{}, fmt.Errorf("service listener %q protocol %q is unsupported", resource.ID, resource.Protocol)
		}
		profile, err := credentialProfile(resource.ID, resource.TLS)
		if err != nil {
			return CompiledIntent{}, err
		}
		name := ownedName("tcp-listener", resource.ID)
		endpoint := qdr.TcpEndpoint{Name: name, Host: resource.Host, Port: strconv.Itoa(int(resource.Port)), SiteId: intent.Target.SiteUID, SslProfile: profile, Observer: resource.Observer, AuthenticatePeer: resource.TLS.Mode == routercontrol.TLSModeMutual}
		result.ResourceNames[resource.ID] = []string{name}
		if len(resource.RoutingKeys) == 1 {
			endpoint.Address = resource.RoutingKeys[0]
		} else {
			endpoint.MultiAddressStrategy = "priority"
			for index, key := range resource.RoutingKeys {
				addressName := ownedName("listener-address", routercontrol.ResourceID(string(resource.ID)+"\x00"+key))
				result.Config.Bridges.ListenerAddresses[addressName] = qdr.ListenerAddress{Name: addressName, Address: key, Value: len(resource.RoutingKeys) - 1 - index, Listener: name}
				result.ResourceNames[resource.ID] = append(result.ResourceNames[resource.ID], addressName)
			}
		}
		result.Config.Bridges.TcpListeners[name] = endpoint
	}
	for _, resource := range intent.ServiceConnectors {
		if resource.Protocol != routercontrol.ProtocolTCP {
			return CompiledIntent{}, fmt.Errorf("service connector %q protocol %q is unsupported", resource.ID, resource.Protocol)
		}
		profile, err := credentialProfile(resource.ID, resource.TLS)
		if err != nil {
			return CompiledIntent{}, err
		}
		for _, endpoint := range resource.Endpoints {
			name := ownedName("tcp-connector", routercontrol.ResourceID(string(resource.ID)+"\x00"+endpoint.ID))
			verify := resource.TLS.VerifyHostname
			result.Config.Bridges.TcpConnectors[name] = qdr.TcpEndpoint{Name: name, Host: endpoint.Host, Port: strconv.Itoa(int(endpoint.Port)), Address: resource.RoutingKey, SiteId: intent.Target.SiteUID, SslProfile: profile, VerifyHostname: &verify, ProcessID: endpoint.ID}
			result.ResourceNames[resource.ID] = append(result.ResourceNames[resource.ID], name)
		}
	}
	hash := sha256.New()
	for _, id := range result.CredentialIDs {
		fmt.Fprintf(hash, "%s\x00", id)
	}
	intentDigest, _ := DigestIntent(intent)
	fmt.Fprintf(hash, "%s", intentDigest)
	result.RealizationID = fmt.Sprintf("%x", hash.Sum(nil))
	return result, nil
}
