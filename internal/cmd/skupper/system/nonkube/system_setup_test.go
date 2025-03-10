package nonkube

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/skupperproject/skupper/internal/cmd/skupper/common"
	"github.com/skupperproject/skupper/internal/cmd/skupper/common/testutils"
	"github.com/skupperproject/skupper/internal/config"
	"github.com/skupperproject/skupper/internal/nonkube/bootstrap"
	"github.com/skupperproject/skupper/pkg/nonkube/api"
	"gotest.tools/v3/assert"
)

func TestCmdSystemSetup_ValidateInput(t *testing.T) {
	type test struct {
		name          string
		args          []string
		flags         *common.CommandSystemSetupFlags
		expectedError string
	}

	testTable := []test{
		{
			name:          "args-are-not-accepted",
			args:          []string{"something"},
			expectedError: "this command does not accept arguments",
		},
		{
			name: "invalid-bundle-strategy",
			flags: &common.CommandSystemSetupFlags{
				Strategy: "not-valid",
			},
			expectedError: "invalid bundle strategy: not-valid",
		},
	}

	for _, test := range testTable {
		t.Run(test.name, func(t *testing.T) {

			command := &CmdSystemSetup{}
			command.CobraCmd = common.ConfigureCobraCommand(common.PlatformLinux, common.SkupperCmdDescription{}, command, nil)

			if test.flags != nil {
				command.Flags = test.flags
			}

			testutils.CheckValidateInput(t, command, test.expectedError, test.args)
		})
	}
}

func TestCmdSystemSetup_InputToOptions(t *testing.T) {

	type test struct {
		name                   string
		args                   []string
		flags                  common.CommandSystemSetupFlags
		platform               string
		namespace              string
		expectedBinary         string
		expectedNamespace      string
		expectedIsBundle       bool
		expectedBundleStrategy string
	}

	testTable := []test{
		{
			name:              "options-by-default",
			flags:             common.CommandSystemSetupFlags{},
			expectedBinary:    "podman",
			expectedNamespace: "default",
		},
		{
			name: "linux",
			flags: common.CommandSystemSetupFlags{
				Path: "input-path",
			},
			namespace:         "east",
			platform:          "linux",
			expectedBinary:    "skrouterd",
			expectedNamespace: "east",
		},
		{
			name: "docker",
			flags: common.CommandSystemSetupFlags{
				Path: "input-path",
			},
			namespace:         "east",
			platform:          "docker",
			expectedBinary:    "docker",
			expectedNamespace: "east",
		},
		{
			name: "bundle-default",
			flags: common.CommandSystemSetupFlags{
				Path:     "input-path",
				Strategy: "bundle",
			},
			namespace:              "east",
			platform:               "podman",
			expectedBinary:         "",
			expectedNamespace:      "east",
			expectedIsBundle:       true,
			expectedBundleStrategy: "bundle",
		},
	}

	for _, test := range testTable {
		t.Run(test.name, func(t *testing.T) {
			os.Setenv(common.ENV_PLATFORM, test.platform)
			config.ClearPlatform()

			cmd := newCmdSystemSetupWithMocks(false, false)
			cmd.Flags = &test.flags
			cmd.Namespace = test.namespace

			cmd.InputToOptions()

			assert.Check(t, cmd.ConfigBootstrap.Binary == test.expectedBinary)
			assert.Check(t, cmd.ConfigBootstrap.BundleStrategy == test.expectedBundleStrategy)
			assert.Check(t, cmd.ConfigBootstrap.Namespace == test.expectedNamespace)
			assert.Check(t, cmd.ConfigBootstrap.IsBundle == test.expectedIsBundle)
			assert.Check(t, strings.Contains(cmd.ConfigBootstrap.InputPath, cmd.Flags.Path))
		})
	}
}

func TestCmdSystemSetup_Run(t *testing.T) {
	type test struct {
		name           string
		preCheckFails  bool
		bootstrapFails bool
		errorMessage   string
	}

	testTable := []test{
		{
			name:           "runs ok",
			preCheckFails:  false,
			bootstrapFails: false,
			errorMessage:   "",
		},
		{
			name:           "pre check fails",
			preCheckFails:  true,
			bootstrapFails: false,
			errorMessage:   "precheck fails",
		},
		{
			name:           "bootstrap fails",
			preCheckFails:  false,
			bootstrapFails: true,
			errorMessage:   "Failed to bootstrap: bootstrap fails",
		},
	}

	for _, test := range testTable {
		command := newCmdSystemSetupWithMocks(test.preCheckFails, test.bootstrapFails)

		t.Run(test.name, func(t *testing.T) {

			err := command.Run()
			if err != nil {
				assert.Check(t, test.errorMessage == err.Error())
			} else {
				assert.Check(t, err == nil)
			}
		})
	}
}

// --- helper methods

func newCmdSystemSetupWithMocks(precheckFails bool, bootstrapFails bool) *CmdSystemSetup {

	cmdMock := &CmdSystemSetup{
		PreCheck:  mockCmdSystemSetupPreCheck,
		Bootstrap: mockCmdSystemSetupBootStrap,
		PostExec:  mockCmdSystemSetupPostExec,
	}
	if precheckFails {
		cmdMock.PreCheck = mockCmdSystemSetupPreCheckFails
	}

	if bootstrapFails {
		cmdMock.Bootstrap = mockCmdSystemSetupBootStrapFails
	}

	return cmdMock
}

func mockCmdSystemSetupPreCheck(config *bootstrap.Config) error { return nil }
func mockCmdSystemSetupPreCheckFails(config *bootstrap.Config) error {
	return fmt.Errorf("precheck fails")
}
func mockCmdSystemSetupBootStrap(config *bootstrap.Config) (*api.SiteState, error) {
	return &api.SiteState{}, nil
}
func mockCmdSystemSetupBootStrapFails(config *bootstrap.Config) (*api.SiteState, error) {
	return nil, fmt.Errorf("bootstrap fails")
}
func mockCmdSystemSetupPostExec(config *bootstrap.Config, siteState *api.SiteState) {}
