package adaptor

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/routercontrol"
)

type credentialRevision struct {
	realization CredentialRealization
	directory   string
}

// SecretCredentialProvider resolves only explicit references from accepted
// intent. It never lists or watches all Secrets in the namespace.
type SecretCredentialProvider struct {
	secrets   corev1client.SecretInterface
	root      string
	mu        sync.Mutex
	revisions map[routercontrol.ResourceID]map[string]credentialRevision
}

func NewSecretCredentialProvider(secrets corev1client.SecretInterface, root string) *SecretCredentialProvider {
	return &SecretCredentialProvider{secrets: secrets, root: root, revisions: map[routercontrol.ResourceID]map[string]credentialRevision{}}
}

func (p *SecretCredentialProvider) Resolve(ctx context.Context, binding routercontrol.CredentialBinding) (CredentialRealization, error) {
	if binding.Provider != routercontrol.CredentialProviderKubernetesSecret {
		return CredentialRealization{}, fmt.Errorf("credential %q uses unsupported provider %q", binding.ID, binding.Provider)
	}
	for _, usage := range binding.Usages {
		if usage != routercontrol.CredentialUsageTrust && usage != routercontrol.CredentialUsageServerAuth && usage != routercontrol.CredentialUsageClientAuth && usage != routercontrol.CredentialUsageProxy {
			return CredentialRealization{}, fmt.Errorf("credential %q uses unsupported usage %q", binding.ID, usage)
		}
	}
	if binding.Reference == "" || strings.Contains(binding.Reference, "/") || filepath.Base(binding.Reference) != binding.Reference {
		return CredentialRealization{}, fmt.Errorf("credential %q has invalid Secret reference", binding.ID)
	}
	secret, err := p.secrets.Get(ctx, binding.Reference, metav1.GetOptions{})
	if err != nil {
		return CredentialRealization{}, err
	}
	ca, cert, key := secret.Data["ca.crt"], secret.Data["tls.crt"], secret.Data["tls.key"]
	isProxy := slices.Contains(binding.Usages, routercontrol.CredentialUsageProxy)
	isTLS := slices.Contains(binding.Usages, routercontrol.CredentialUsageTrust) || slices.Contains(binding.Usages, routercontrol.CredentialUsageClientAuth) || slices.Contains(binding.Usages, routercontrol.CredentialUsageServerAuth)
	if isProxy && (len(secret.Data["host"]) == 0 || len(secret.Data["port"]) == 0) {
		return CredentialRealization{}, fmt.Errorf("proxy credential %q requires host and port", binding.ID)
	}
	if isTLS {
		if err := validateTrafficCredential(binding, ca, cert, key); err != nil {
			return CredentialRealization{}, err
		}
	}
	hash := sha256.New()
	for _, name := range []string{"ca.crt", "tls.crt", "tls.key", "host", "port", "username", "password"} {
		value := secret.Data[name]
		fmt.Fprintf(hash, "%s:%d:", name, len(value))
		hash.Write(value)
	}
	id := hex.EncodeToString(hash.Sum(nil))
	p.mu.Lock()
	defer p.mu.Unlock()
	if revisions := p.revisions[binding.ID]; revisions != nil {
		if revision, ok := revisions[id]; ok {
			return revision.realization, nil
		}
	}
	directory := filepath.Join(p.root, ownedName("credential", binding.ID), id)
	if err := materializeCredential(directory, ca, cert, key); err != nil {
		return CredentialRealization{}, err
	}
	result := CredentialRealization{RealizationID: id}
	if isTLS {
		result.Profile = qdr.SslProfile{CaCertFile: filepath.Join(directory, "ca.crt")}
	}
	if isProxy {
		result.ProxyProfile = &qdr.ProxyProfile{Host: string(secret.Data["host"]), Port: string(secret.Data["port"]), Username: string(secret.Data["username"]), Password: string(secret.Data["password"])}
	}
	if isTLS && len(cert) > 0 {
		result.Profile.CertFile = filepath.Join(directory, "tls.crt")
		result.Profile.PrivateKeyFile = filepath.Join(directory, "tls.key")
	}
	if p.revisions[binding.ID] == nil {
		p.revisions[binding.ID] = map[string]credentialRevision{}
	}
	p.revisions[binding.ID][id] = credentialRevision{realization: result, directory: directory}
	return result, nil
}

func validateTrafficCredential(binding routercontrol.CredentialBinding, ca, cert, key []byte) error {
	pool := x509.NewCertPool()
	if len(ca) == 0 || !pool.AppendCertsFromPEM(ca) {
		return fmt.Errorf("credential %q has no valid ca.crt", binding.ID)
	}
	requiresKeypair := slices.Contains(binding.Usages, routercontrol.CredentialUsageClientAuth) || slices.Contains(binding.Usages, routercontrol.CredentialUsageServerAuth)
	if requiresKeypair && (len(cert) == 0 || len(key) == 0) {
		return fmt.Errorf("credential %q requires tls.crt and tls.key", binding.ID)
	}
	if len(cert) != 0 || len(key) != 0 {
		if _, err := tls.X509KeyPair(cert, key); err != nil {
			return fmt.Errorf("credential %q has invalid key pair: %w", binding.ID, err)
		}
	}
	return nil
}

func materializeCredential(directory string, ca, cert, key []byte) error {
	parent := filepath.Dir(directory)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(parent, ".credential-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(temporary)
		}
	}()
	for _, file := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{{"ca.crt", ca, 0644}, {"tls.crt", cert, 0644}, {"tls.key", key, 0600}} {
		if len(file.data) == 0 {
			continue
		}
		if err := os.WriteFile(filepath.Join(temporary, file.name), file.data, file.mode); err != nil {
			return err
		}
	}
	if err := os.Rename(temporary, directory); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	ok = true
	return nil
}

// Retire removes material only after callers prove no live QDR consumer uses it.
func (p *SecretCredentialProvider) Retire(live map[string]struct{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for binding, revisions := range p.revisions {
		for id, revision := range revisions {
			if _, active := live[id]; active {
				continue
			}
			if err := os.RemoveAll(revision.directory); err != nil {
				return err
			}
			delete(revisions, id)
		}
		if len(revisions) == 0 {
			delete(p.revisions, binding)
		}
	}
	return nil
}
