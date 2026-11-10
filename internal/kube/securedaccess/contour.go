package securedaccess

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func extractEndpoint(obj *unstructured.Unstructured) skupperv2alpha1.Endpoint {
	_, portName := getKeyAndPortNameForHttpProxy(obj)
	return skupperv2alpha1.Endpoint{
		Name: portName,
		Host: extractHost(obj),
		Port: "443",
	}
}

func getKeyAndPortNameForHttpProxy(o *unstructured.Unstructured) (string, string) {
	svcName := extractServiceName(o)
	portName := strings.TrimPrefix(o.GetName(), svcName+"-")
	return o.GetNamespace() + "/" + svcName, portName
}

func httpProxyName(svcName string, portName string) string {
	return svcName + "-" + portName
}

var httpProxyResource = schema.GroupVersionResource{
	Group:    "projectcontour.io",
	Version:  "v1",
	Resource: "httpproxies",
}
var httpProxyGVK = schema.GroupVersionKind{
	Group:   httpProxyResource.Group,
	Version: httpProxyResource.Version,
	Kind:    "HTTPProxy",
}

type HttpProxy struct {
	Name        string
	Host        string
	ServiceName string
	ServicePort int
}

func desiredHttpProxies(domain string, access *skupperv2alpha1.SecuredAccess) []HttpProxy {
	var proxies []HttpProxy
	for _, port := range access.Spec.Ports {
		name := httpProxyName(access.Name, port.Name)
		proxies = append(proxies, HttpProxy{
			Name:        name,
			Host:        qualify(name, domain),
			ServiceName: access.Name,
			ServicePort: port.Port,
		})
	}
	return proxies
}

func extractHost(obj *unstructured.Unstructured) string {
	host, _, _ := unstructured.NestedString(obj.UnstructuredContent(), "spec", "virtualhost", "fqdn")
	return host
}

func extractServiceName(obj *unstructured.Unstructured) string {
	proxy := &HttpProxy{}
	proxy.readFromContourProxy(obj)
	return proxy.ServiceName
}

func (p *HttpProxy) readFromContourProxy(obj *unstructured.Unstructured) error {
	host, _, err := unstructured.NestedString(obj.UnstructuredContent(), "spec", "virtualhost", "fqdn")
	if err != nil {
		return err
	}
	p.Host = host
	services, _, err := unstructured.NestedSlice(obj.UnstructuredContent(), "spec", "tcpproxy", "services")
	if err != nil {
		return err
	}
	if len(services) > 0 {
		service, ok := services[0].(map[string]interface{})
		if ok {
			name, _, err := unstructured.NestedString(service, "name")
			if err != nil {
				return err
			}
			port, _, err := unstructured.NestedInt64(service, "port")
			if err != nil {
				return err
			}
			p.ServiceName = name
			p.ServicePort = int(port)
		}
	}
	return nil
}

func (p *HttpProxy) writeToContourProxy(obj *unstructured.Unstructured) error {
	err := unstructured.SetNestedField(obj.UnstructuredContent(), p.Host, "spec", "virtualhost", "fqdn")
	if err != nil {
		return err
	}
	err = unstructured.SetNestedField(obj.UnstructuredContent(), true, "spec", "virtualhost", "tls", "passthrough")
	if err != nil {
		return err
	}
	services := []interface{}{
		map[string]interface{}{
			"name": p.ServiceName,
			"port": int64(p.ServicePort),
		},
	}
	err = unstructured.SetNestedSlice(obj.UnstructuredContent(), services, "spec", "tcpproxy", "services")
	if err != nil {
		return err
	}
	return nil
}
