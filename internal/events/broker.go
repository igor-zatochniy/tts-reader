package events

import (
	"sync"
	"time"
)

const eventBufferSize = 32

type Options[T any] struct {
	IsLossy      func(T) bool
	Sequence     func(T) uint64
	WithMetadata func(T, uint64, time.Time) T
}

type Broker[T any] struct {
	mu      sync.Mutex
	clients map[chan T]struct{}
	nextSeq uint64
	options Options[T]
}

func NewBroker[T any](options Options[T]) *Broker[T] {
	return &Broker[T]{
		clients: make(map[chan T]struct{}),
		options: options,
	}
}

func (b *Broker[T]) Subscribe() (<-chan T, func()) {
	return b.subscribe(nil)
}

// SubscribeWithInitial додає клієнта та початкову подію під одним блокуванням.
func (b *Broker[T]) SubscribeWithInitial(initial T) (<-chan T, func()) {
	return b.subscribe(&initial)
}

func (b *Broker[T]) subscribe(initial *T) (<-chan T, func()) {
	ch := make(chan T, eventBufferSize)
	b.mu.Lock()
	if initial != nil {
		event := *initial
		if b.options.Sequence != nil && b.options.Sequence(event) == 0 {
			event = b.newEventLocked(event)
		} else if b.options.Sequence != nil && b.options.Sequence(event) > b.nextSeq {
			b.nextSeq = b.options.Sequence(event)
		}
		ch <- event
	}
	b.clients[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if _, ok := b.clients[ch]; ok {
			delete(b.clients, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

func (b *Broker[T]) NewEvent(event T) T {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.newEventLocked(event)
}

func (b *Broker[T]) Publish(event T) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.options.Sequence != nil && b.options.Sequence(event) == 0 {
		event = b.newEventLocked(event)
	} else {
		if b.options.Sequence != nil {
			if seq := b.options.Sequence(event); seq > b.nextSeq {
				b.nextSeq = seq
			}
		}
	}
	b.publishLocked(event)
}

func (b *Broker[T]) publishLocked(event T) {
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
			if b.options.IsLossy != nil && b.options.IsLossy(event) {
				continue
			}
			delete(b.clients, ch)
			close(ch)
		}
	}
}

func (b *Broker[T]) newEventLocked(event T) T {
	b.nextSeq++
	if b.options.WithMetadata == nil {
		return event
	}
	return b.options.WithMetadata(event, b.nextSeq, time.Now().UTC())
}
