package controllerruntime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/grants"
	"github.com/skupperproject/skupper/internal/kube/leadership"
)

// grantGenerator buffers credentials until live authority and the Site used to
// issue them have been revalidated. Nothing is released after loss of authority.
func grantGenerator(ctx context.Context, clients internalclient.Clients, lookup grants.SiteLookup, active *atomic.Pointer[leadership.Fence]) grants.GrantResponse {
	return func(namespace, name, subject string, writer io.Writer) error {
		fence := active.Load()
		if fence == nil {
			return grants.ErrNotLeader
		}
		if err := fence.Check(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		site, err := lookup(ctx, namespace)
		if err != nil {
			return err
		}
		generator, err := grants.NewTokenGeneratorContext(ctx, site, clients)
		if err != nil {
			return err
		}
		token, err := generator.NewCertToken(name, subject)
		if err != nil {
			return err
		}
		var buffer bytes.Buffer
		if err := token.Write(&buffer); err != nil {
			return err
		}
		current, err := lookup(ctx, namespace)
		if err != nil {
			return err
		}
		if current.UID != site.UID || current.ResourceVersion != site.ResourceVersion {
			return fmt.Errorf("Site changed during grant issuance")
		}
		if err := fence.Check(); err != nil {
			return err
		}
		_, err = buffer.WriteTo(writer)
		return err
	}
}
