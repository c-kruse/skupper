package grants

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

const (
	redemptionResponseKey = "response"
	redemptionSiteUIDKey  = "site-uid"
)

func RedeemAccessToken(token *skupperv2alpha1.AccessToken, site *skupperv2alpha1.Site, clients internalclient.Clients) error {
	return RedeemAccessTokenContext(context.Background(), token, site, clients, nil)
}

func RedeemAccessTokenContext(ctx context.Context, token *skupperv2alpha1.AccessToken, site *skupperv2alpha1.Site, clients internalclient.Clients, beforeEffects func(context.Context) error) error {
	if token.Spec.Ca == "" {
		err := fmt.Errorf("token does not have a CA")
		return updateAccessTokenStatusContext(ctx, token, err, clients, beforeEffects)
	}
	transport := &http.Transport{
		TLSClientConfig: tlsConfig(token),
	}
	body, err := postTokenRequestContext(ctx, token, site, transport)
	if err != nil {
		return updateAccessTokenStatusContext(ctx, token, err, clients, beforeEffects)
	}
	slog.Info("HTTP Post was successful, decoding response body",
		slog.String("URL", token.Spec.Url),
		slog.String("namespace", token.Namespace),
		slog.String("name", token.Name))
	if beforeEffects != nil {
		if err := beforeEffects(ctx); err != nil {
			return err
		}
	}
	return handleTokenResponseContext(ctx, body, token, site, clients, beforeEffects)
}

func requestAccessTokenContext(ctx context.Context, token *skupperv2alpha1.AccessToken, site *skupperv2alpha1.Site) ([]byte, error) {
	if token.Spec.Ca == "" {
		return nil, fmt.Errorf("token does not have a CA")
	}
	body, err := postTokenRequestContext(ctx, token, site, &http.Transport{TLSClientConfig: tlsConfig(token)})
	if err != nil {
		return nil, err
	}
	return io.ReadAll(body)
}

func tlsConfig(token *skupperv2alpha1.AccessToken) *tls.Config {
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM([]byte(token.Spec.Ca))
	return &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS13,
	}
}

func postTokenRequest(token *skupperv2alpha1.AccessToken, site *skupperv2alpha1.Site, transport http.RoundTripper) (io.Reader, error) {
	return postTokenRequestContext(context.Background(), token, site, transport)
}

func postTokenRequestContext(ctx context.Context, token *skupperv2alpha1.AccessToken, site *skupperv2alpha1.Site, transport http.RoundTripper) (io.Reader, error) {
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	url, err := url.Parse(token.Spec.Url)
	if err != nil {
		return nil, fmt.Errorf("could not parse token url: %w", err)
	}
	if url.Scheme != "https" {
		return nil, fmt.Errorf("token url scheme must be https")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, token.Spec.Url, bytes.NewReader([]byte(token.Spec.Code)))
	if err != nil {
		return nil, fmt.Errorf("Controller got error: %s", err)
	}
	request.Header.Add("name", token.Name)
	request.Header.Add("subject", string(site.ObjectMeta.UID))
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Controller got error: %s", err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if readErr != nil {
		return nil, fmt.Errorf("Controller could not read response: %s", readErr)
	}
	if len(body) > 1024*1024 {
		return nil, fmt.Errorf("Controller response exceeds size limit")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Controller got failed response: %d (%s) %s", response.StatusCode, http.StatusText(response.StatusCode), strings.TrimSpace(string(body)))
	}
	return bytes.NewReader(body), nil
}

func handleTokenResponse(body io.Reader, token *skupperv2alpha1.AccessToken, site *skupperv2alpha1.Site, clients internalclient.Clients) error {
	return handleTokenResponseContext(context.Background(), body, token, site, clients, nil)
}

func handleTokenResponseContext(ctx context.Context, body io.Reader, token *skupperv2alpha1.AccessToken, site *skupperv2alpha1.Site, clients internalclient.Clients, beforeEffects func(context.Context) error) error {
	decoder := newLinkDecoder(body)
	if err := decoder.decodeAll(); err != nil {
		slog.Error("Could not decode response for AccessToken",
			slog.String("namespace", token.Namespace),
			slog.String("name", token.Name),
			slog.Any("error", err))
		return updateAccessTokenStatusContext(ctx, token, errors.New("Controller could not decode response"), clients, beforeEffects)
	}
	refs := []metav1.OwnerReference{
		{
			Kind:       "Site",
			APIVersion: "skupper.io/v2alpha1",
			Name:       site.Name,
			UID:        site.ObjectMeta.UID,
		},
	}
	decoder.secret.ObjectMeta.OwnerReferences = refs
	if err := authorizeEffect(ctx, beforeEffects); err != nil {
		return err
	}
	if err := ensureResponseSecret(ctx, token.Namespace, &decoder.secret, clients); err != nil {
		return updateAccessTokenStatusContext(ctx, token, fmt.Errorf("Controller could not create received secret: %s", err), clients, beforeEffects)
	}
	for _, link := range decoder.links {
		link.ObjectMeta.OwnerReferences = refs
		if token.Spec.LinkCost > 0 {
			link.Spec.Cost = token.Spec.LinkCost
		}
		if err := authorizeEffect(ctx, beforeEffects); err != nil {
			return err
		}
		if err := ensureResponseLink(ctx, token.Namespace, &link, clients); err != nil {
			return updateAccessTokenStatusContext(ctx, token, fmt.Errorf("Controller could not create received link: %s", err), clients, beforeEffects)
		}
	}

	return updateAccessTokenStatusContext(ctx, token, nil, clients, beforeEffects)
}

func updateAccessTokenStatus(token *skupperv2alpha1.AccessToken, err error, clients internalclient.Clients) error {
	return updateAccessTokenStatusContext(context.Background(), token, err, clients, nil)
}

func updateAccessTokenStatusContext(ctx context.Context, token *skupperv2alpha1.AccessToken, err error, clients internalclient.Clients, beforeEffects func(context.Context) error) error {
	if token.SetRedeemed(err) {
		if gateErr := authorizeEffect(ctx, beforeEffects); gateErr != nil {
			return gateErr
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err = clients.GetSkupperClient().SkupperV2alpha1().AccessTokens(token.ObjectMeta.Namespace).UpdateStatus(ctx, token, metav1.UpdateOptions{})
		return err
	}
	return nil
}

func authorizeEffect(ctx context.Context, authorize func(context.Context) error) error {
	if authorize != nil {
		return authorize(ctx)
	}
	return nil
}

func redemptionResponseName(token *skupperv2alpha1.AccessToken) string {
	return "skupper-redemption-" + string(token.UID)
}

func redemptionResponseOwner(token *skupperv2alpha1.AccessToken) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "skupper.io/v2alpha1",
		Kind:       "AccessToken",
		Name:       token.Name,
		UID:        token.UID,
	}
}

func saveRedemptionResponse(ctx context.Context, token *skupperv2alpha1.AccessToken, siteUID types.UID, body []byte, clients internalclient.Clients) error {
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            redemptionResponseName(token),
			Namespace:       token.Namespace,
			OwnerReferences: []metav1.OwnerReference{redemptionResponseOwner(token)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			redemptionResponseKey: body,
			redemptionSiteUIDKey:  []byte(siteUID),
		},
	}
	return ensureResponseSecret(ctx, token.Namespace, desired, clients)
}

func loadRedemptionResponse(ctx context.Context, token *skupperv2alpha1.AccessToken, clients internalclient.Clients) ([]byte, types.UID, bool, error) {
	secret, err := clients.GetKubeClient().CoreV1().Secrets(token.Namespace).Get(ctx, redemptionResponseName(token), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	if !reflect.DeepEqual(secret.OwnerReferences, []metav1.OwnerReference{redemptionResponseOwner(token)}) {
		return nil, "", false, fmt.Errorf("stored redemption response ownership changed")
	}
	body, ok := secret.Data[redemptionResponseKey]
	if !ok {
		return nil, "", false, fmt.Errorf("stored redemption response is missing")
	}
	siteUID := types.UID(secret.Data[redemptionSiteUIDKey])
	if siteUID == "" {
		return nil, "", false, fmt.Errorf("stored redemption response Site UID is missing")
	}
	return body, siteUID, true, nil
}

func ensureResponseSecret(ctx context.Context, namespace string, desired *corev1.Secret, clients internalclient.Clients) error {
	_, err := clients.GetKubeClient().CoreV1().Secrets(namespace).Create(ctx, desired, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, getErr := clients.GetKubeClient().CoreV1().Secrets(namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	if !reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) || !reflect.DeepEqual(existing.Data, desired.Data) || normalizedSecretType(existing.Type) != normalizedSecretType(desired.Type) {
		return err
	}
	return nil
}

func normalizedSecretType(secretType corev1.SecretType) corev1.SecretType {
	if secretType == "" {
		return corev1.SecretTypeOpaque
	}
	return secretType
}

func ensureResponseLink(ctx context.Context, namespace string, desired *skupperv2alpha1.Link, clients internalclient.Clients) error {
	_, err := clients.GetSkupperClient().SkupperV2alpha1().Links(namespace).Create(ctx, desired, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, getErr := clients.GetSkupperClient().SkupperV2alpha1().Links(namespace).Get(ctx, desired.Name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	if !reflect.DeepEqual(existing.OwnerReferences, desired.OwnerReferences) || !reflect.DeepEqual(existing.Spec, desired.Spec) {
		return err
	}
	return nil
}

type LinkDecoder struct {
	decoder *yaml.YAMLOrJSONDecoder
	secret  corev1.Secret
	links   []skupperv2alpha1.Link
}

func newLinkDecoder(r io.Reader) *LinkDecoder {
	return &LinkDecoder{
		decoder: yaml.NewYAMLOrJSONDecoder(r, 1024),
	}
}

func (d *LinkDecoder) decodeSecret() error {
	return d.decoder.Decode(&d.secret)
}

func (d *LinkDecoder) decodeLink() error {
	var link skupperv2alpha1.Link
	if err := d.decoder.Decode(&link); err != nil {
		return err
	}
	d.links = append(d.links, link)
	return nil
}

func (d *LinkDecoder) decodeAll() error {
	if err := d.decodeSecret(); err != nil {
		return err
	}
	for {
		err := d.decodeLink()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
