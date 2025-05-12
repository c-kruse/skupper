package sslprofile

import (
	"errors"
	"strings"

	"github.com/skupperproject/skupper/internal/qdr"
	"github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

func RouterAccess(provider Provider, ra *v2alpha1.RouterAccess) (string, error) {
	if ra == nil {
		return "", errors.New("missing RouterAccess definition")
	}
	return provider.Get(ra.Spec.TlsCredentials, Opts{})
}

func Link(provider Provider, link *v2alpha1.Link) (string, error) {
	if link == nil {
		return "", errors.New("missing Link definition")
	}
	return provider.Get(link.Spec.TlsCredentials, Opts{NamingStrategy: UseProfileSuffix})
}

// Provider for sslprofile configuration
type Provider interface {
	qdr.ConfigUpdate
	// Get returns the name for an sslprofile configuration or an error when
	// the Provider cannot configure the profile.
	Get(tlsCredentials string, opts Opts) (string, error)
}

type Opts struct {
	NamingStrategy NamingStrategy
	CAOnly         bool
}

type NamingStrategy interface {
	ProfileToTLSCredentials(profileName string) string
	TLSCredentialsToProfile(tlsCredentails string) string
}

const (
	UseProfileSuffix = suffixNamer("-profile")
	UseDefaultName   = identityNamer("")
)

type identityNamer string

func (identityNamer) ProfileToTLSCredentials(s string) string { return s }
func (identityNamer) TLSCredentialsToProfile(s string) string { return s }

type suffixNamer string

func (n suffixNamer) ProfileToTLSCredentials(s string) string {
	return strings.TrimSuffix(s, string(n))
}
func (n suffixNamer) TLSCredentialsToProfile(s string) string { return s + string(n) }
