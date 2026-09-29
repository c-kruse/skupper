package grants

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/watchers"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
)

const (
	redemptionAttemptAnnotation = "internal.skupper.io/redemption-attempt-uid"
	redemptionSiteAnnotation    = "internal.skupper.io/redemption-site-uid"
)

var ErrNotLeader = errors.New("grant service is not the active leader")

type EffectGate interface {
	Check() error
	Done() <-chan struct{}
}

type SiteLookup func(context.Context, string) (*skupperv2alpha1.Site, error)

type ServiceOptions struct {
	Clients          internalclient.Clients
	CurrentNamespace string
	WatchNamespace   string
	Config           *GrantConfig
	Generator        GrantResponse
	LookupSite       SiteLookup
	IsControlled     NamespaceFilter
}

type Service struct {
	clients          internalclient.Clients
	events           *watchers.EventProcessor
	currentNamespace string
	config           *GrantConfig
	lookupSite       SiteLookup
	isControlled     NamespaceFilter
	enabled          *GrantsEnabled
	disabled         *GrantsDisabled

	mu        sync.RWMutex
	gate      EffectGate
	leaderCtx context.Context
	leader    bool
	started   bool
}

func NewService(options ServiceOptions) (*Service, error) {
	if options.Clients == nil || options.Config == nil {
		return nil, fmt.Errorf("grant service clients and config are required")
	}
	events := watchers.NewEventProcessor("GrantService", options.Clients)
	service := &Service{clients: options.Clients, events: events, currentNamespace: options.CurrentNamespace, config: options.Config, lookupSite: options.LookupSite, isControlled: options.IsControlled}
	if options.Config.Enabled {
		service.enabled = newEnabled(events, options.CurrentNamespace, options.WatchNamespace, options.Config, options.Generator, options.IsControlled, true)
		service.enabled.grants.authorize = service.checkAuthority
		service.enabled.grants.effectContext = service.apiContext
	} else {
		service.disabled = &GrantsDisabled{clients: events, logger: slog.Default()}
		events.WatchAccessGrants(options.WatchNamespace, service.markGrantDisabled)
	}
	events.WatchAccessTokens(options.WatchNamespace, service.checkAccessToken)
	return service, nil
}

// StartCaches starts informers only. Their handlers remain queued until RunLeader.
func (s *Service) StartCaches(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("grant service caches already started")
	}
	s.started = true
	s.events.StartWatchers(ctx.Done())
	return nil
}

func (s *Service) WaitForCacheSync(ctx context.Context) bool {
	return s.events.WaitForCacheSync(ctx.Done())
}

// Prepare performs leader-only installation effects. It must be called only
// after election acquisition and before opening readiness.
func (s *Service) Prepare(ctx context.Context) error {
	if s.enabled == nil || s.enabled.autoConfigure == nil {
		return nil
	}
	return s.enabled.autoConfigure.configure(ctx, s.clients, s.currentNamespace)
}

// RunLeader processes warmed events and serves grants until leadership is lost.
// The EventProcessor is terminal after return, matching the process-exit contract.
func (s *Service) RunLeader(ctx context.Context, gate EffectGate) error {
	if gate == nil {
		return fmt.Errorf("grant service leader gate is required")
	}
	if err := gate.Check(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
		case <-gate.Done():
		case <-runCtx.Done():
		}
		cancel()
	}()
	s.mu.Lock()
	s.gate = gate
	s.leaderCtx = runCtx
	s.leader = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.leader = false
		s.leaderCtx = nil
		s.mu.Unlock()
		s.events.Stop()
	}()

	// Initial informer Add events were queued while caches warmed. Starting the
	// processor drains them independently, so one invalid resource cannot block
	// grant serving or the rest of the initial state.
	s.events.Start(runCtx.Done())
	if s.enabled == nil {
		<-runCtx.Done()
		return runCtx.Err()
	}
	if s.enabled.autoConfigure != nil {
		select {
		case <-s.enabled.serveReady:
		case <-runCtx.Done():
			return runCtx.Err()
		}
	}
	return s.enabled.server.run(runCtx, gate)
}

func (s *Service) checkAuthority() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.leader || s.gate == nil {
		return ErrNotLeader
	}
	return s.gate.Check()
}

func (s *Service) apiContext() (context.Context, context.CancelFunc) {
	s.mu.RLock()
	ctx := s.leaderCtx
	s.mu.RUnlock()
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, 10*time.Second)
}

func (s *Service) controlled(namespace string) bool {
	return s.isControlled == nil || s.isControlled(namespace)
}

func (s *Service) markGrantDisabled(key string, grant *skupperv2alpha1.AccessGrant) error {
	if err := s.checkAuthority(); err != nil {
		return err
	}
	if grant == nil || !s.controlled(grant.Namespace) {
		return nil
	}
	ctx, cancel := s.apiContext()
	defer cancel()
	return s.disabled.markGrantNotEnabledContext(ctx, key, grant)
}

func (s *Service) checkAccessToken(_ string, token *skupperv2alpha1.AccessToken) error {
	if token == nil || token.IsRedeemed() || !s.controlled(token.Namespace) {
		return nil
	}
	if err := s.checkAuthority(); err != nil {
		return err
	}
	s.mu.RLock()
	leaderCtx := s.leaderCtx
	s.mu.RUnlock()
	if leaderCtx == nil {
		return ErrNotLeader
	}
	ctx, cancel := context.WithTimeout(leaderCtx, 30*time.Second)
	defer cancel()
	live, err := s.clients.GetSkupperClient().SkupperV2alpha1().AccessTokens(token.Namespace).Get(ctx, token.Name, metav1.GetOptions{})
	if err != nil || live.UID != token.UID || live.IsRedeemed() {
		return err
	}
	if live.Annotations[redemptionAttemptAnnotation] == string(live.UID) {
		attemptSiteUID := types.UID(live.Annotations[redemptionSiteAnnotation])
		if attemptSiteUID == "" {
			return s.markRedemptionUnknown(ctx, live)
		}
		body, stagedSiteUID, found, err := loadRedemptionResponse(ctx, live, s.clients)
		if err != nil {
			return err
		}
		if found {
			if stagedSiteUID != attemptSiteUID {
				return fmt.Errorf("stored redemption response Site ownership changed")
			}
			if s.lookupSite == nil {
				return fmt.Errorf("site lookup is required for token redemption")
			}
			site, err := s.lookupSite(ctx, live.Namespace)
			if err != nil || site == nil {
				return err
			}
			if site.UID != attemptSiteUID {
				return fmt.Errorf("Site ownership changed")
			}
			return handleTokenResponseContext(ctx, bytes.NewReader(body), live, site, s.clients, s.redemptionAuthority(live, attemptSiteUID))
		}
		return s.markRedemptionUnknown(ctx, live)
	}
	if s.lookupSite == nil {
		return fmt.Errorf("site lookup is required for token redemption")
	}
	site, err := s.lookupSite(ctx, live.Namespace)
	if err != nil || site == nil {
		return err
	}
	parsedURL, err := url.Parse(live.Spec.Url)
	if live.Spec.Ca == "" || err != nil || parsedURL.Scheme != "https" {
		return RedeemAccessTokenContext(ctx, live, site, s.clients, func(context.Context) error { return s.checkAuthority() })
	}
	beforeEffects := s.redemptionAuthority(live, site.UID)
	if err := beforeEffects(ctx); err != nil {
		return err
	}
	live = live.DeepCopy()
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	live.Annotations[redemptionAttemptAnnotation] = string(live.UID)
	live.Annotations[redemptionSiteAnnotation] = string(site.UID)
	live, err = s.clients.GetSkupperClient().SkupperV2alpha1().AccessTokens(live.Namespace).Update(ctx, live, metav1.UpdateOptions{})
	if err != nil {
		return err // No remote request occurred; informer conflict may retry safely.
	}
	if err := beforeEffects(ctx); err != nil {
		return nil // Marker is durable; a successor reports Unknown without POST.
	}
	body, err := requestAccessTokenContext(ctx, live, site)
	if err != nil {
		if authorityErr := beforeEffects(ctx); authorityErr != nil {
			return nil
		}
		_ = updateAccessTokenStatusContext(ctx, live, err, s.clients, beforeEffects)
		return nil // The request may have reached the peer; never replay it.
	}
	if err := beforeEffects(ctx); err != nil {
		return nil
	}
	if err := saveRedemptionResponse(ctx, live, site.UID, body, s.clients); err != nil {
		return nil // A create with an ambiguous result is recovered by UID on the next leader.
	}
	_ = handleTokenResponseContext(ctx, bytes.NewReader(body), live, site, s.clients, beforeEffects)
	return nil // Never cause an automatic replay after the remote POST started.
}

func (s *Service) redemptionAuthority(token *skupperv2alpha1.AccessToken, siteUID types.UID) func(context.Context) error {
	return func(effectCtx context.Context) error {
		if err := s.checkAuthority(); err != nil || !s.controlled(token.Namespace) {
			return ErrNotLeader
		}
		currentToken, err := s.clients.GetSkupperClient().SkupperV2alpha1().AccessTokens(token.Namespace).Get(effectCtx, token.Name, metav1.GetOptions{})
		if err != nil || currentToken.UID != token.UID {
			return fmt.Errorf("AccessToken ownership changed")
		}
		currentSite, err := s.lookupSite(effectCtx, token.Namespace)
		if err != nil || currentSite == nil || currentSite.UID != siteUID {
			return fmt.Errorf("Site ownership changed")
		}
		return nil
	}
}

func (s *Service) markRedemptionUnknown(ctx context.Context, token *skupperv2alpha1.AccessToken) error {
	if err := s.checkAuthority(); err != nil {
		return err
	}
	token = token.DeepCopy()
	message := "Redemption outcome is unknown; replace the AccessToken to retry"
	changed := token.Status.StatusType != skupperv2alpha1.StatusPending || token.Status.Message != message
	token.Status.StatusType = skupperv2alpha1.StatusPending
	token.Status.Message = message
	if token.Status.SetCondition("Redeemed", skupperv2alpha1.ConditionState{Status: metav1.ConditionUnknown, Reason: skupperv2alpha1.StatusPending, Message: message}, token.Generation) {
		changed = true
	}
	if !changed {
		return nil
	}
	_, err := s.clients.GetSkupperClient().SkupperV2alpha1().AccessTokens(token.Namespace).UpdateStatus(ctx, token, metav1.UpdateOptions{})
	return err
}
