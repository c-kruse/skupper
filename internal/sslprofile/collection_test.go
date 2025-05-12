package sslprofile_test

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/internal/sslprofile"
)

func TestStaticCollection(t *testing.T) {
	const basePath = "/testing/"
	type Args struct {
		TLSCredentials string
		Opts           sslprofile.Opts
		ExpectErr      bool
	}
	testCases := []struct {
		Args   []Args
		Expect map[string]qdr.SslProfile
	}{
		{},
		{
			Args: []Args{
				{TLSCredentials: "my-secret-name"},
				{TLSCredentials: "my-secret-name"},
				{TLSCredentials: "my-secret-name", ExpectErr: true, Opts: sslprofile.Opts{
					CAOnly: true,
				}},
			},
			Expect: map[string]qdr.SslProfile{
				"my-secret-name": {
					Name:           "my-secret-name",
					CertFile:       "/testing/my-secret-name/tls.crt",
					PrivateKeyFile: "/testing/my-secret-name/tls.key",
					CaCertFile:     "/testing/my-secret-name/ca.crt",
				},
			},
		},
		{
			Args: []Args{
				{TLSCredentials: "my-secret-name"},
				{TLSCredentials: "my-secret-name", Opts: sslprofile.Opts{NamingStrategy: sslprofile.UseProfileSuffix}},
				{TLSCredentials: "my-secret-name-profile", ExpectErr: true},
			},
			Expect: map[string]qdr.SslProfile{
				"my-secret-name": {
					Name:           "my-secret-name",
					CertFile:       "/testing/my-secret-name/tls.crt",
					PrivateKeyFile: "/testing/my-secret-name/tls.key",
					CaCertFile:     "/testing/my-secret-name/ca.crt",
				},
				"my-secret-name-profile": {
					Name:           "my-secret-name-profile",
					CertFile:       "/testing/my-secret-name-profile/tls.crt",
					PrivateKeyFile: "/testing/my-secret-name-profile/tls.key",
					CaCertFile:     "/testing/my-secret-name-profile/ca.crt",
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run("", func(t *testing.T) {

			config := qdr.RouterConfig{
				SslProfiles: make(map[string]qdr.SslProfile),
				Listeners:   make(map[string]qdr.Listener),
			}
			references := make(map[string]struct{})
			provider := sslprofile.NewStaticCollection(basePath)
			for i, arg := range tc.Args {
				ref, err := provider.Get(arg.TLSCredentials, arg.Opts)
				if arg.ExpectErr && err == nil {
					t.Errorf("expected provider to return error %d", i)
				} else if !arg.ExpectErr && err != nil {
					t.Errorf("provider returned unexpected error %d: %s", i, err)
				}
				if _, ok := references[ref]; !ok && err == nil {
					config.Listeners[fmt.Sprintf("lstnr/%s", ref)] = qdr.Listener{
						SslProfile: ref,
					}
				}
			}
			config.SslProfiles = make(map[string]qdr.SslProfile)
			provider.Apply(&config)
			if delta := cmp.Diff(tc.Expect, config.SslProfiles, cmpopts.EquateEmpty()); delta != "" {
				t.Error(delta)
			}
		})
	}

}
