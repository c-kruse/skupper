//go:build integration

package controllerruntime

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	v2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
)

// TestKindGrants requires grant autoconfiguration enabled with local access on
// the disposable controller. Credentials stay in Kubernetes and are never
// printed. The issuing HA Site and redeeming edge Site are existing fixtures.
func TestKindGrants(t *testing.T) {
	kubeconfig := os.Getenv("SKUPPER_CONTROL_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("set SKUPPER_CONTROL_TEST_KUBECONFIG for the disposable kind fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rest, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	clients, err := internalclient.NewClientFromRestConfig(rest, "reconciliation-default")
	if err != nil {
		t.Fatal(err)
	}
	kube, api := clients.GetKubeClient(), clients.GetSkupperClient().SkupperV2alpha1()
	const issuing, redeeming = "reconciliation-ha", "reconciliation-default"
	name := "grant-live-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	grants, tokens, links := api.AccessGrants(issuing), api.AccessTokens(redeeming), api.Links(redeeming)
	if _, err := grants.Create(ctx, &v2alpha1.AccessGrant{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v2alpha1.AccessGrantSpec{RedemptionsAllowed: 1, ExpirationWindow: "5m"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer grants.Delete(context.Background(), name, metav1.DeleteOptions{})
	var tokenSpec v2alpha1.AccessTokenSpec
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		grant, err := grants.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if !grant.IsReady() || grant.Status.Ca == "" || grant.Status.Code == "" || grant.Status.Url == "" {
			return false, nil
		}
		tokenSpec = v2alpha1.AccessTokenSpec{Url: grant.Status.Url, Code: grant.Status.Code, Ca: grant.Status.Ca, LinkCost: 7}
		return true, nil
	}); err != nil {
		t.Fatalf("autoconfigured grant did not become ready: %v", err)
	}
	// Links and credentials are Site-owned rather than AccessToken-owned. Remove
	// those test effects explicitly, including partial results after a failure.
	defer func() {
		current, err := links.List(context.Background(), metav1.ListOptions{})
		if err == nil {
			for _, link := range current.Items {
				if strings.HasPrefix(link.Name, name+"-") {
					_ = links.Delete(context.Background(), link.Name, metav1.DeleteOptions{})
				}
			}
		}
		_ = kube.CoreV1().Secrets(redeeming).Delete(context.Background(), name, metav1.DeleteOptions{})
	}()
	token, err := tokens.Create(ctx, &v2alpha1.AccessToken{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: tokenSpec}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tokens.Delete(context.Background(), name, metav1.DeleteOptions{})
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		current, err := tokens.Get(ctx, name, metav1.GetOptions{})
		return err == nil && current.IsRedeemed(), err
	}); err != nil {
		current, _ := tokens.Get(ctx, name, metav1.GetOptions{})
		if current != nil {
			t.Logf("redemption status: %s", current.Status.StatusType)
		}
		t.Fatalf("AccessToken did not redeem: %v", err)
	}
	staged, err := kube.CoreV1().Secrets(redeeming).Get(ctx, "skupper-redemption-"+string(token.UID), metav1.GetOptions{})
	if err != nil || len(staged.Data["response"]) == 0 {
		t.Fatalf("successful redemption response was not durably staged: %v", err)
	}
	created, err := links.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]bool{}
	for _, link := range created.Items {
		if !strings.HasPrefix(link.Name, name+"-") {
			continue
		}
		if link.Spec.Cost != 7 || link.Spec.TlsCredentials != name || len(link.Spec.Endpoints) != 2 {
			t.Fatal("redemption lost Link cost, credential reference, or HA endpoint pair")
		}
		for _, endpoint := range link.Spec.Endpoints {
			groups[endpoint.Group] = true
		}
	}
	if !groups["skupper-router"] || !groups["skupper-router-2"] || len(groups) != 2 {
		t.Fatal("redemption did not create both HA Link groups")
	}
	// A distinct token UID using the consumed grant must not issue credentials.
	second, err := tokens.Create(ctx, &v2alpha1.AccessToken{ObjectMeta: metav1.ObjectMeta{Name: name + "-spent"}, Spec: tokenSpec}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer tokens.Delete(context.Background(), second.Name, metav1.DeleteOptions{})
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		current, err := tokens.Get(ctx, second.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if current.IsRedeemed() {
			t.Fatal("spent grant was redeemed twice")
		}
		for _, condition := range current.Status.Conditions {
			if condition.Type == v2alpha1.CONDITION_TYPE_REDEEMED && condition.Status == metav1.ConditionUnknown {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("spent grant did not reach terminal Unknown: %v", err)
	}
	grant, err := grants.Get(ctx, name, metav1.GetOptions{})
	if err != nil || grant.Status.Redemptions != 1 {
		t.Fatalf("grant redemption limit was not preserved: %v", err)
	}
	if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		current, err := links.List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, err
		}
		ready := 0
		for _, link := range current.Items {
			if strings.HasPrefix(link.Name, name+"-") && link.IsReady() {
				ready++
			}
		}
		return ready == 2, nil
	}); err != nil {
		t.Fatalf("generated dual-role HA Links did not both connect from the edge Site: %v", err)
	}
	t.Log("standalone grant TLS/autoconfiguration served HTTPS; one redemption durably staged credentials and two operational HA Links with cost 7; spent grant produced terminal Unknown without a second redemption")
}
