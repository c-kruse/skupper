//go:build performance && !integration
// +build performance,!integration

package performance

import (
	"testing"

	"github.com/c-kruse/skupper/test/integration/performance/common"
)

func TestMain(m *testing.M) {
	common.RunPerformanceTests(m, false)
}
