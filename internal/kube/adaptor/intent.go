package adaptor

import "github.com/skupperproject/skupper/internal/routercontrol"

func DigestIntent(intent routercontrol.RouterIntent) (routercontrol.Digest, error) {
	_, digest, err := routercontrol.CanonicalIntent(intent)
	return digest, err
}

func ValidateIntent(intent routercontrol.RouterIntent) error {
	return routercontrol.ValidateIntent(intent)
}
