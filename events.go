package main

import (
	"sync"
	"time"
)

type PlaybackEvent struct {
	Type     string           `json:"type"`
	Time     time.Time        `json:"time"`
	Playback PlaybackSnapshot `json:"playback"`
}

type EventBroker struct {
	mu      sync.Mutex
	clients map[chan PlaybackEvent]struct{}
}

func NewEventBroker() *EventBroker {
	return &EventBroker{clients: make(map[chan PlaybackEvent]struct{})}
}

func (b *EventBroker) Subscribe() (<-chan PlaybackEvent, func()) {
	ch := make(chan PlaybackEvent, 32)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		delete(b.clients, ch)
		close(ch)
		b.mu.Unlock()
	}
}

func (b *EventBroker) Publish(event PlaybackEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
		}
	}
}
