//go:build integration || examples
// +build integration examples

package examples

import (
	"context"
	"testing"

	"github.com/c-kruse/skupper/test/integration/examples/mongodb"
)

func TestMongo(t *testing.T) {
	mongodb.Run(context.Background(), t, testRunner)
}
