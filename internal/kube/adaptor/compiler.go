package adaptor

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strconv"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
	"github.com/skupperproject/skupper/internal/version"
)

const ownedNamePrefix = "skupper.v2."

type CredentialRealization struct {
	Profile       qdr.SslProfile
	ProxyProfile  *qdr.ProxyProfile
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
	if intent.Settings.DataConnectionCount > 0 {
		config.Metadata.DataConnectionCount = strconv.FormatUint(uint64(intent.Settings.DataConnectionCount), 10)
	}
	for _, setting := range intent.Settings.Logging {
		parsed, err := qdr.ParseRouterLogConfig(func() string {
			if setting.Module == "" {
				return setting.Level
			}
			return setting.Module + ":" + setting.Level
		}())
		if err != nil {
			return CompiledIntent{}, err
		}
		qdr.ConfigureRouterLogging(&config, parsed)
	}
	config.AddAddress(qdr.Address{Prefix: "mc", Distribution: "multicast"})
	config.AddHealthAndMetricsListener(9090)
	config.AddListener(qdr.Listener{Name: "amqp", Host: "localhost", Port: 5672})
	result := CompiledIntent{Config: config, ResourceNames: map[routercontrol.ResourceID][]string{}, CredentialIDs: map[routercontrol.ResourceID]string{}}
	bindings := map[routercontrol.ResourceID]routercontrol.CredentialBinding{}
	for _, binding := range intent.CredentialBindings {
		bindings[binding.ID] = binding
	}

	credentialProfile := func(owner routercontrol.ResourceID, tls routercontrol.TLSIntent, usage string) (string, error) {
		if tls.Mode == "" || tls.Mode == routercontrol.TLSModeDisabled {
			return "", nil
		}
		if !slices.Contains(bindings[tls.CredentialBinding].Usages, usage) {
			return "", fmt.Errorf("credential %q for resource %q lacks %q usage", tls.CredentialBinding, owner, usage)
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
		profile, err := credentialProfile(resource.ID, resource.TLS, routercontrol.CredentialUsageClientAuth)
		if err != nil {
			return CompiledIntent{}, err
		}
		name := ownedName("connection", resource.ID)
		connector := qdr.Connector{Name: name, Host: resource.Host, Port: strconv.Itoa(int(resource.Port)), Role: qdr.Role(resource.Role), Cost: int32(resource.Cost), SslProfile: profile, VerifyHostname: resource.TLS.VerifyHostname}
		if resource.ProxyCredentialBinding != "" {
			if !slices.Contains(bindings[resource.ProxyCredentialBinding].Usages, routercontrol.CredentialUsageProxy) {
				return CompiledIntent{}, fmt.Errorf("credential %q for resource %q lacks proxy usage", resource.ProxyCredentialBinding, resource.ID)
			}
			proxy, found := credentials[resource.ProxyCredentialBinding]
			if !found || proxy.ProxyProfile == nil {
				return CompiledIntent{}, fmt.Errorf("proxy credential %q for resource %q is unavailable", resource.ProxyCredentialBinding, resource.ID)
			}
			profile := *proxy.ProxyProfile
			profile.Name = ownedName("proxy", resource.ProxyCredentialBinding)
			result.Config.ProxyProfiles[profile.Name] = profile
			connector.ProxyProfile = profile.Name
			result.CredentialIDs[resource.ProxyCredentialBinding] = proxy.RealizationID
		}
		result.Config.AddConnector(connector)
		result.ResourceNames[resource.ID] = []string{name}
	}
	for _, resource := range intent.RouterListeners {
		profile, err := credentialProfile(resource.ID, resource.TLS, routercontrol.CredentialUsageServerAuth)
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
		profile, err := credentialProfile(resource.ID, resource.TLS, routercontrol.CredentialUsageServerAuth)
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
		profile, err := credentialProfile(resource.ID, resource.TLS, routercontrol.CredentialUsageClientAuth)
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
	credentialKeys := make([]string, 0, len(result.CredentialIDs))
	for binding := range result.CredentialIDs {
		credentialKeys = append(credentialKeys, string(binding))
	}
	slices.Sort(credentialKeys)
	for _, binding := range credentialKeys {
		fmt.Fprintf(hash, "%s:%s\x00", binding, result.CredentialIDs[routercontrol.ResourceID(binding)])
	}
	intentDigest, _ := DigestIntent(intent)
	fmt.Fprintf(hash, "%s", intentDigest)
	result.RealizationID = fmt.Sprintf("%x", hash.Sum(nil))
	return result, nil
}
