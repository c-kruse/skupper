package resource

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/lru"
)

// SSASuccessCache remembers successful real SSA requests, not predicted equality.
// Use one cache per API cluster and fixed execution policy (main resource,
// non-forcing apply). Restart or eviction merely causes another server evaluation.
// There is no TTL or automatic admission-policy invalidation; Clear explicitly
// when a policy change requires reevaluating otherwise unchanged requests.
type SSASuccessCache struct{ entries *lru.Cache }

func NewSSASuccessCache(capacity int) *SSASuccessCache {
	return &SSASuccessCache{entries: lru.New(capacity)}
}

type ssaRequestKey struct {
	GVK                          schema.GroupVersionKind
	Namespace, Name, Manager, RV string
	UID                          types.UID
	Intent                       [sha256.Size]byte
}

func ssaKey(observed, desired *unstructured.Unstructured, manager string) (ssaRequestKey, error) {
	if desired == nil || manager == "" {
		return ssaRequestKey{}, fmt.Errorf("desired and manager are required")
	}
	if observed.GroupVersionKind() != desired.GroupVersionKind() || observed.GetName() != desired.GetName() || observed.GetNamespace() != desired.GetNamespace() {
		return ssaRequestKey{}, fmt.Errorf("observation identity does not match desired")
	}
	if observed.GetUID() == "" || observed.GetResourceVersion() == "" {
		return ssaRequestKey{}, fmt.Errorf("observation needs UID and resourceVersion")
	}
	// encoding/json sorts map keys; list ordering is intentionally not normalized.
	raw, err := json.Marshal(desired.Object)
	if err != nil {
		return ssaRequestKey{}, err
	}
	return ssaRequestKey{desired.GroupVersionKind(), desired.GetNamespace(), desired.GetName(), manager, observed.GetResourceVersion(), observed.GetUID(), sha256.Sum256(raw)}, nil
}

// Include performs no I/O and does not mutate either input. A hit means this
// exact intent already succeeded at this observed incarnation/revision, not
// that running admission again would produce the same result. Desired contains
// authored intent only, without executor-added UID/RV guards or observed defaults.
func (c *SSASuccessCache) Include(observed, desired *unstructured.Unstructured, manager string) (bool, error) {
	if desired == nil || manager == "" {
		return false, fmt.Errorf("desired and manager are required")
	}
	if observed == nil {
		return true, nil
	}
	k, err := ssaKey(observed, desired, manager)
	if err != nil {
		return false, err
	}
	_, hit := c.entries.Get(k)
	return !hit, nil
}

// Record is called ONLY after a successful non-dry-run apply. Supply the actual
// response and the authored payload sent, before executor-added UID/RV guards.
// Never record errors, timeouts with unknown outcome, or a dry-run response.
func (c *SSASuccessCache) Record(written, desired *unstructured.Unstructured, manager string) error {
	if written == nil {
		return fmt.Errorf("successful response required")
	}
	k, err := ssaKey(written, desired, manager)
	if err != nil {
		return err
	}
	c.entries.Add(k, struct{}{})
	return nil
}

func (c *SSASuccessCache) Clear() { c.entries.Clear() }
