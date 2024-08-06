package collector

import (
	"strings"
	"testing"

	"gotest.tools/assert"
)

func TestIDer(t *testing.T) {
	idp := newStableIdentityProvider()
	assert.Check(t, idp.ID("test", "a", "bbbbb", "cccc") != idp.ID("foo", "a", "bbbbb", "cccc"))
	assert.Equal(t, idp.ID("a", "x"), newStableIdentityProvider().ID("a", "x"))
	assert.Check(t, strings.HasPrefix(idp.ID("test", "a", "b"), "test-"))
}
