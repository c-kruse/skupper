package resource

import (
	"fmt"
	"sync"
	"testing"

	"gotest.tools/v3/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSSASuccessCache(t *testing.T) {
	for _, kind := range []string{"Gateway", "ServiceAccount"} {
		t.Run(kind, func(t *testing.T) {
			desired := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "v1", "kind": kind,
				"metadata": map[string]interface{}{"namespace": "test", "name": "example"},
			}}
			if kind == "Gateway" {
				desired.SetAPIVersion("gateway.networking.k8s.io/v1")
			}
			desired.SetAnnotations(map[string]string{"admission": "requested"})
			observed := desired.DeepCopy()
			observed.SetUID("uid-a")
			observed.SetResourceVersion("before")
			written := observed.DeepCopy()
			written.SetResourceVersion("after")
			// Model admission rewriting an authored value: a receipt remembers
			// server acceptance, not literal equality with the request.
			written.SetAnnotations(map[string]string{"admission": "admitted"})
			cache := NewSSASuccessCache(1024)
			decision := func(o, d *unstructured.Unstructured, manager string, want bool) {
				t.Helper()
				include, err := cache.Include(o, d, manager)
				assert.NilError(t, err)
				assert.Equal(t, include, want)
			}
			decision(nil, desired, "manager", true)
			decision(written, desired, "manager", true)
			assert.NilError(t, cache.Record(written, desired, "manager"))
			decision(written, desired, "manager", false)
			decision(observed, desired, "manager", true) // Cache the response RV, not the request's RV.
			decision(written, desired, "other-manager", true)
			for _, change := range []func(*unstructured.Unstructured){
				func(o *unstructured.Unstructured) { o.SetResourceVersion("status-only-change") },
				func(o *unstructured.Unstructured) { o.SetUID("uid-b") },
			} {
				other := written.DeepCopy()
				change(other)
				decision(other, desired, "manager", true)
			}
			changed := desired.DeepCopy()
			changed.SetAnnotations(map[string]string{"intent": "new"})
			decision(written, changed, "manager", true)
			// Unknown observation fields are not parsed or compared. This is a
			// synthetic version-skew observation, not newer-server evidence.
			written.Object["futureField"] = "unknown"
			decision(written, desired, "manager", false)
			before := written.DeepCopy()
			assert.NilError(t, cache.Record(written, desired, "manager"))
			decision(written, desired, "manager", false)
			assert.DeepEqual(t, written, before)
			cache.Clear()
			decision(written, desired, "manager", true)
			assert.NilError(t, NewSSASuccessCache(1024).Record(written, desired, "manager"))
			decision(written, desired, "manager", true) // Receipts do not cross instances/clusters.
		})
	}
}

func TestSSASuccessCacheRejectsInvalidIdentity(t *testing.T) {
	desired := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "ServiceAccount"}}
	desired.SetName("example")
	desired.SetNamespace("test")
	written := desired.DeepCopy()
	written.SetUID("uid-a")
	written.SetResourceVersion("revision")
	cache := NewSSASuccessCache(1024)
	for _, change := range []func(*unstructured.Unstructured){
		func(o *unstructured.Unstructured) { o.SetName("other") },
		func(o *unstructured.Unstructured) { o.SetNamespace("other") },
		func(o *unstructured.Unstructured) { o.SetKind("Service") },
		func(o *unstructured.Unstructured) { o.SetAPIVersion("other/v1") },
		func(o *unstructured.Unstructured) { o.SetUID("") },
		func(o *unstructured.Unstructured) { o.SetResourceVersion("") },
	} {
		other := written.DeepCopy()
		change(other)
		_, err := cache.Include(other, desired, "manager")
		assert.Assert(t, err != nil)
		assert.Assert(t, cache.Record(other, desired, "manager") != nil)
	}
	_, err := cache.Include(nil, nil, "manager")
	assert.Assert(t, err != nil)
	_, err = cache.Include(written, desired, "")
	assert.Assert(t, err != nil)
	assert.Assert(t, cache.Record(nil, desired, "manager") != nil)
	include, err := cache.Include(written, desired, "manager")
	assert.NilError(t, err)
	assert.Assert(t, include, "invalid receipts must not establish success")
}

func TestSSASuccessCacheEvictionAndConcurrency(t *testing.T) {
	for _, capacity := range []int{2, 1024, 1025} { // small, Access default, and above default
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			desired := &unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": "v1", "kind": "ServiceAccount"}}
			desired.SetName("example")
			written := desired.DeepCopy()
			written.SetUID("uid-a")
			written.SetResourceVersion("revision")
			cache := NewSSASuccessCache(capacity)
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					for range 20 {
						if err := cache.Record(written, desired, "manager"); err != nil {
							t.Error(err)
						}
						if include, err := cache.Include(written, desired, "manager"); err != nil || include {
							t.Errorf("expected concurrent receipt hit: include=%v err=%v", include, err)
						}
					}
				})
			}
			wg.Wait()
			for i := range capacity {
				o, d := written.DeepCopy(), desired.DeepCopy()
				d.SetName(fmt.Sprintf("other-%d", i))
				o.SetName(d.GetName())
				assert.NilError(t, cache.Record(o, d, "manager"))
			}
			include, err := cache.Include(written, desired, "manager")
			assert.NilError(t, err)
			assert.Assert(t, include, "capacity+1 entries must evict the oldest receipt")
			desired.SetName("other-0")
			written.SetName(desired.GetName())
			include, err = cache.Include(written, desired, "manager")
			assert.NilError(t, err)
			assert.Assert(t, !include, "the configured capacity must retain all newer receipts")
		})
	}
}
