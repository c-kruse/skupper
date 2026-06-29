package adaptor

import (
	"context"
	"testing"

	"github.com/skupperproject/skupper/internal/network"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperfake "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned/fake"
	"gotest.tools/v3/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplySiteNetworkStatus(t *testing.T) {
	site := &skupperv2alpha1.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "site1", Namespace: "test"},
	}
	client := skupperfake.NewSimpleClientset(site)

	info := network.NetworkStatusInfo{
		SiteStatus: []network.SiteStatusInfo{
			{Site: network.SiteInfo{Identity: "id-1", Name: "site1", Namespace: "test", Platform: "kubernetes", Version: "1.8.0"}},
			{Site: network.SiteInfo{Identity: "id-2", Name: "east", Namespace: "test", Platform: "podman", Version: "1.8.0"}},
		},
	}

	err := applySiteNetworkStatus(context.Background(), client, "test", "site1", info)
	assert.NilError(t, err)

	updated, err := client.SkupperV2alpha1().Sites("test").Get(context.Background(), "site1", metav1.GetOptions{})
	assert.NilError(t, err)
	assert.Equal(t, updated.Status.SitesInNetwork, 2)
	assert.Equal(t, len(updated.Status.Network), 2)
}
