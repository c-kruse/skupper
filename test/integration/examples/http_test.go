//go:build integration || examples
// +build integration examples

package examples

import (
	"context"
	"testing"

	"github.com/c-kruse/skupper/test/integration/examples/http"
	"github.com/c-kruse/skupper/test/utils/constants"
)

func TestHttp(t *testing.T) {
	ctx, cancelFn := context.WithTimeout(context.Background(), constants.ImagePullingAndResourceCreationTimeout)
	defer cancelFn()
	http.Run(ctx, t, testRunner)
}
