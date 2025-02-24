package resources

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

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

func resourceTemplates(site *skupperv2alpha1.Site, group string, size sizing.Sizing) []resource.Template {
	options := getCoreParams(site, group, size)
	templates := []resource.Template{
		{
			Name:       "deployment",
			Template:   routerDeploymentTemplate,
			Parameters: options,
			Resource: schema.GroupVersionResource{
				Group:    "apps",
				Version:  "v1",
				Resource: "deployments",
			},
		},
		{
			Name:       "localService",
			Template:   routerLocalServiceTemplate,
			Parameters: options,
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
	SiteId         string
	SiteName       string
	Group          string
	Replicas       int
	ServiceAccount string
	ConfigDigest   string
	RouterImage    skuppertypes.ImageDetails
	AdaptorImage   skuppertypes.ImageDetails
	Sizing         sizing.Sizing
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

func getCoreParams(site *skupperv2alpha1.Site, group string, size sizing.Sizing) CoreParams {
	return CoreParams{
		SiteId:         site.GetSiteId(),
		SiteName:       site.Name,
		Group:          group,
		Replicas:       1,
		ServiceAccount: site.Spec.GetServiceAccount(),
		ConfigDigest:   configDigest(&site.Spec),
		RouterImage:    images.GetRouterImageDetails(),
		AdaptorImage:   images.GetKubeAdaptorImageDetails(),
		Sizing:         size,
	}
}

func Apply(clients internalclient.Clients, ctx context.Context, site *skupperv2alpha1.Site, group string, size sizing.Sizing) error {
	for _, t := range resourceTemplates(site, group, size) {
		_, err := t.Apply(clients.GetDynamicClient(), ctx, site.Namespace)
		if err != nil {
			return err
		}
	}
	return nil
}
