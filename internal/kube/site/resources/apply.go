package resources

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"path/filepath"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	skuppertypes "github.com/skupperproject/skupper/api/types"
	"github.com/skupperproject/skupper/internal/images"
	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/resource"
	"github.com/skupperproject/skupper/internal/kube/site/sizing"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

//go:embed skupper-router-deployment.yaml
var routerDeploymentTemplate string

//go:embed skupper-router-local-service.yaml
var routerLocalServiceTemplate string

type Labelling interface {
	SetLabels(namespace string, name string, kind string, labels map[string]string) bool
	SetAnnotations(namespace string, name string, kind string, annotations map[string]string) bool
	SetObjectMetadata(namespace string, name string, kind string, meta *metav1.ObjectMeta) bool
	SetPodObjectMetadata(namespace string, name string, kind string, meta *metav1.ObjectMeta) bool
}

func resourceTemplates(site *skupperv2alpha1.Site, group string, size sizing.Sizing, labelling Labelling, disableSecCtx bool, routerControl *RouterControlConfig) []resource.Template {
	templates := []resource.Template{
		{
			Name:       "deployment",
			Template:   routerDeploymentTemplate,
			Parameters: getCoreParams(site, group, size, disableSecCtx, routerControl).setLabelsAndAnnotations(labelling, site.Namespace, "skupper-router", "Deployment"),
			Resource: schema.GroupVersionResource{
				Group:    "apps",
				Version:  "v1",
				Resource: "deployments",
			},
		},
		{
			Name:       "localService",
			Template:   routerLocalServiceTemplate,
			Parameters: getCoreParams(site, group, size, disableSecCtx, routerControl).setLabelsAndAnnotations(labelling, site.Namespace, "skupper-router-local", "Service"),
			Resource: schema.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "services",
			},
		},
	}
	return templates
}

type CoreParams struct {
	SiteId             string
	SiteName           string
	Group              string
	Replicas           int
	ServiceAccount     string
	ConfigDigest       string
	RouterImage        skuppertypes.ImageDetails
	AdaptorImage       skuppertypes.ImageDetails
	Sizing             sizing.Sizing
	Labels             map[string]string
	Annotations        map[string]string
	PodLabels          map[string]string
	PodAnnotations     map[string]string
	EnableAntiAffinity bool
	DisableSecCtx      bool
	RouterControl      *RouterControlConfig
}

type RouterControlConfig struct {
	NamespaceUID      string
	SiteUID           string
	EnrollmentURL     string
	ControlAddress    string
	TLSServerName     string
	TokenAudience     string
	TokenPath         string
	TokenFile         string
	CABundleConfigMap string
	CABundleKey       string
	CABundlePath      string
	CABundleFile      string
	TokenDirectory    string
	CABundleDirectory string
}

func (p *CoreParams) setLabelsAndAnnotations(labelling Labelling, namespace string, name string, kind string) *CoreParams {
	if labelling == nil {
		return p
	}
	p.Labels = map[string]string{}
	p.Annotations = map[string]string{}
	meta := &metav1.ObjectMeta{
		Labels:      p.Labels,
		Annotations: p.Annotations,
	}
	labelling.SetObjectMetadata(namespace, name, kind, meta)
	quoteValues(p.Labels)
	quoteValues(p.Annotations)
	if kind == "Deployment" {
		p.PodLabels = map[string]string{}
		p.PodAnnotations = map[string]string{}
		podMeta := &metav1.ObjectMeta{
			Labels:      p.PodLabels,
			Annotations: p.PodAnnotations,
		}
		labelling.SetPodObjectMetadata(namespace, name, kind, podMeta)
		quoteValues(p.PodLabels)
		quoteValues(p.PodAnnotations)
	}
	return p
}

func quoteValues(items map[string]string) {
	for key, value := range items {
		items[key] = quoted(value)
	}
}

func quoted(in string) string {
	if numeric(in) || boolean(in) {
		return "\"" + in + "\""
	}
	if needsBlockQuote(in) {
		return "|\n      " + in
	}
	return in
}

func numeric(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

func boolean(s string) bool {
	_, err := strconv.ParseBool(s)
	return err == nil
}

func needsBlockQuote(s string) bool {
	// is there a better way to test if block quoting is required?
	test := "x: " + s
	o := map[string]string{}
	err := yaml.Unmarshal([]byte(test), &o)
	if err != nil {
		return true
	}
	return false
}

type Resources struct {
	Requests map[string]string
	Limits   map[string]string
}

func (r Resources) NotEmpty() bool {
	return len(r.Requests) > 0 || len(r.Limits) > 0
}

func configDigest(config *skupperv2alpha1.SiteSpec) string {
	if config != nil {
		// add any values from spec which require a router restart if changed:
		h := sha256.New()
		if config.Edge {
			h.Write([]byte("edge"))
		} else {
			h.Write([]byte("interior"))
		}
		if dcc := config.GetRouterDataConnectionCount(); dcc != "" {
			h.Write([]byte(dcc))
		}
		if logging := config.GetRouterLogging(); logging != "" {
			h.Write([]byte(logging))
		}
		return fmt.Sprintf("%x", h.Sum(nil))
	}
	return ""
}

func getCoreParams(site *skupperv2alpha1.Site, group string, size sizing.Sizing, disableSecCtx bool, routerControl *RouterControlConfig) *CoreParams {
	if routerControl != nil {
		copy := *routerControl
		copy.TokenDirectory = filepath.Dir(copy.TokenPath)
		copy.TokenFile = filepath.Base(copy.TokenPath)
		copy.CABundleDirectory = filepath.Dir(copy.CABundlePath)
		copy.CABundleFile = filepath.Base(copy.CABundlePath)
		routerControl = &copy
	}
	return &CoreParams{
		SiteId:             site.GetSiteId(),
		SiteName:           site.Name,
		Group:              group,
		Replicas:           1,
		ServiceAccount:     site.Spec.GetServiceAccount(),
		ConfigDigest:       configDigest(&site.Spec),
		RouterImage:        images.GetRouterImageDetails(),
		AdaptorImage:       images.GetKubeAdaptorImageDetails(),
		Sizing:             size,
		Labels:             map[string]string{},
		EnableAntiAffinity: enableAntiAffinity(site),
		DisableSecCtx:      disableSecCtx,
		RouterControl:      routerControl,
	}
}

func Apply(clients internalclient.Clients, ctx context.Context, site *skupperv2alpha1.Site, group string, size sizing.Sizing, labelling Labelling, disableSecCtx bool) error {
	return apply(clients, ctx, site, group, size, labelling, disableSecCtx, nil)
}

// ApplyWithRouterControl renders the single-authority router-control bootstrap.
// It does not mount or reference a router-config ConfigMap.
func ApplyWithRouterControl(clients internalclient.Clients, ctx context.Context, site *skupperv2alpha1.Site, group string, size sizing.Sizing, labelling Labelling, disableSecCtx bool, routerControl RouterControlConfig) error {
	return apply(clients, ctx, site, group, size, labelling, disableSecCtx, &routerControl)
}

// RenderWithRouterControl deterministically renders the workload objects that
// ApplyWithRouterControl would send to SSA, without requiring a client.
func RenderWithRouterControl(site *skupperv2alpha1.Site, group string, size sizing.Sizing, labelling Labelling, disableSecCtx bool, routerControl RouterControlConfig) (*appsv1.Deployment, *corev1.Service, error) {
	templates := resourceTemplates(site, group, size, labelling, disableSecCtx, &routerControl)
	deployment := &appsv1.Deployment{}
	service := &corev1.Service{}
	for _, candidate := range templates {
		object, err := candidate.Render(site.Namespace)
		if err != nil {
			return nil, nil, err
		}
		var target runtime.Object
		switch candidate.Resource.Resource {
		case "deployments":
			target = deployment
		case "services":
			target = service
		default:
			return nil, nil, fmt.Errorf("unsupported router workload resource %q", candidate.Resource.Resource)
		}
		data, err := object.MarshalJSON()
		if err != nil {
			return nil, nil, err
		}
		if err := yaml.Unmarshal(data, target); err != nil {
			return nil, nil, err
		}
	}
	return deployment, service, nil
}

func apply(clients internalclient.Clients, ctx context.Context, site *skupperv2alpha1.Site, group string, size sizing.Sizing, labelling Labelling, disableSecCtx bool, routerControl *RouterControlConfig) error {
	for _, t := range resourceTemplates(site, group, size, labelling, disableSecCtx, routerControl) {
		_, err := t.Apply(clients.GetDynamicClient(), ctx, site.Namespace)
		if err != nil {
			return err
		}
	}
	return nil
}

func enableAntiAffinity(site *skupperv2alpha1.Site) bool {
	return site.Spec.HA && !getValueAsBool(site.Spec.Settings, "disable-anti-affinity")
}

func getValueAsBool(settings map[string]string, key string) bool {
	if settings == nil {
		return false
	}
	if sval, ok := settings[key]; ok {
		bval, _ := strconv.ParseBool(sval)
		return bval
	}
	return false
}
