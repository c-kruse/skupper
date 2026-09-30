package routercontrol

import "sync"

type publishedIntent struct {
	intent    RouterIntent
	canonical []byte
	digest    Digest
	available bool
	revision  uint64
}

// Publisher stores the newest immutable intent per target and wakes sessions
// through a size-one notification channel. Publication never waits for a
// network consumer.
type Publisher struct {
	mu          sync.RWMutex
	states      map[TargetIdentity]publishedIntent
	subscribers map[TargetIdentity]map[chan struct{}]struct{}
}

func NewPublisher() *Publisher {
	return &Publisher{
		states:      map[TargetIdentity]publishedIntent{},
		subscribers: map[TargetIdentity]map[chan struct{}]struct{}{},
	}
}

func (p *Publisher) Publish(intent RouterIntent) (Digest, error) {
	canonical, digest, err := CanonicalIntent(intent)
	if err != nil {
		return "", err
	}
	normalized, err := DecodeIntent(canonical, digest)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	previous := p.states[normalized.Target]
	if previous.available && previous.digest == digest {
		p.mu.Unlock()
		return digest, nil
	}
	p.states[normalized.Target] = publishedIntent{
		intent: normalized, canonical: append([]byte(nil), canonical...), digest: digest,
		available: true, revision: previous.revision + 1,
	}
	p.notifyLocked(normalized.Target)
	p.mu.Unlock()
	return digest, nil
}

func (p *Publisher) SetUnavailable(target TargetIdentity) {
	p.mu.Lock()
	previous := p.states[target]
	if !previous.available && previous.revision != 0 {
		p.mu.Unlock()
		return
	}
	p.states[target] = publishedIntent{revision: previous.revision + 1}
	p.notifyLocked(target)
	p.mu.Unlock()
}

// PublishedIntents returns an isolated view of this publisher's current desired
// state. A new leader's empty publisher has no evidence of earlier publication.
func (p *Publisher) PublishedIntents(namespaceUID string) map[TargetIdentity]Publication {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make(map[TargetIdentity]Publication)
	for target, state := range p.states {
		if target.NamespaceUID == namespaceUID {
			result[target] = Publication{Digest: state.digest, Available: state.available, Revision: state.revision}
		}
	}
	return result
}

func (p *Publisher) notifyLocked(target TargetIdentity) {
	for subscriber := range p.subscribers[target] {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (p *Publisher) current(target TargetIdentity) publishedIntent {
	p.mu.RLock()
	state := p.states[target]
	p.mu.RUnlock()
	state.canonical = append([]byte(nil), state.canonical...)
	if state.available {
		state.intent = deepCopyIntent(state.intent)
	}
	return state
}

func (p *Publisher) subscribe(target TargetIdentity) (<-chan struct{}, func()) {
	updates := make(chan struct{}, 1)
	p.mu.Lock()
	if p.subscribers[target] == nil {
		p.subscribers[target] = map[chan struct{}]struct{}{}
	}
	p.subscribers[target][updates] = struct{}{}
	p.mu.Unlock()
	cancel := func() {
		p.mu.Lock()
		delete(p.subscribers[target], updates)
		if len(p.subscribers[target]) == 0 {
			delete(p.subscribers, target)
		}
		p.mu.Unlock()
	}
	return updates, cancel
}
