package sslprofile

import (
	"fmt"
	"path"

	"github.com/skupperproject/skupper/internal/qdr"
)

type profileRef struct {
	SslProfile     qdr.SslProfile
	TlsCredentials string
	CAOnly         bool
}

type staticCollection struct {
	profiles map[string]profileRef
	basePath string
}

func NewStaticCollection(basePath string) Provider {
	return &staticCollection{
		profiles: make(map[string]profileRef),
		basePath: basePath,
	}
}

func (c *staticCollection) Get(tlsCredentials string, opts Opts) (string, error) {
	if opts.NamingStrategy == nil {
		opts.NamingStrategy = UseDefaultName
	}
	profileName := opts.NamingStrategy.TLSCredentialsToProfile(tlsCredentials)
	ref, ok := c.profiles[profileName]
	if !ok {
		ref = profileRef{
			TlsCredentials: tlsCredentials,
			CAOnly:         opts.CAOnly,
			SslProfile: qdr.SslProfile{
				Name:       profileName,
				CaCertFile: path.Join(c.basePath, profileName, "ca.crt"),
			},
		}
		if !opts.CAOnly {
			ref.SslProfile.CertFile = path.Join(c.basePath, profileName, "tls.crt")
			ref.SslProfile.PrivateKeyFile = path.Join(c.basePath, profileName, "tls.key")
		}
		c.profiles[profileName] = ref
	}
	if opts.CAOnly != ref.CAOnly || tlsCredentials != ref.TlsCredentials {
		return profileName, fmt.Errorf("conflicting SslProfiles for tlsCredentials %s", tlsCredentials)
	}
	return profileName, nil
}

func (c *staticCollection) Apply(config *qdr.RouterConfig) bool {
	changed := false
	for profileName, ref := range c.profiles {
		existing, ok := config.SslProfiles[profileName]
		if !ok {
			changed = true
			config.SslProfiles[profileName] = ref.SslProfile
		}
		if ref.SslProfile != existing {
			changed = true
			config.SslProfiles[profileName] = ref.SslProfile
		}
	}
	for name := range config.UnreferencedSslProfiles() {
		config.RemoveSslProfile(name)
		changed = true
	}
	return changed
}
