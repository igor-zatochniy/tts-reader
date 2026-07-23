package core

import (
	"sync"
	"time"
)

const eventBufferSize = 32

type PlaybackEvent struct {
	Seq      uint64           `json:"seq"`
	Type     string           `json:"type"`
	Time     time.Time        `json:"time"`
	Playback PlaybackSnapshot `json:"playback"`
}

type EventBroker struct {
	mu      sync.Mutex
	clients map[chan PlaybackEvent]struct{}
	nextSeq uint64
}

func NewEventBroker() *EventBroker {
	return &EventBroker{clients: make(map[chan PlaybackEvent]struct{})}
}

func (b *EventBroker) Subscribe() (<-chan PlaybackEvent, func()) {
	ch := make(chan PlaybackEvent, eventBufferSize)
	b.mu.Lock()
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

func (b *EventBroker) NewEvent(eventType string, snapshot PlaybackSnapshot) PlaybackEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.newEventLocked(eventType, snapshot)
}

func (b *EventBroker) Publish(event PlaybackEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if event.Seq == 0 {
		event = b.newEventLocked(event.Type, event.Playback)
	} else {
		if event.Time.IsZero() {
			event.Time = time.Now().UTC()
		}
		if event.Seq > b.nextSeq {
			b.nextSeq = event.Seq
		}
	}
	b.publishLocked(event)
}

func (b *EventBroker) PublishPlayback(eventType string, snapshot PlaybackSnapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishLocked(b.newEventLocked(eventType, snapshot))
}

func (b *EventBroker) publishLocked(event PlaybackEvent) {
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
			if isLossyPlaybackEvent(event.Type) {
				continue
			}
			delete(b.clients, ch)
			close(ch)
		}
	}
}

func (b *EventBroker) newEventLocked(eventType string, snapshot PlaybackSnapshot) PlaybackEvent {
	b.nextSeq++
	return PlaybackEvent{
		Seq:      b.nextSeq,
		Type:     eventType,
		Time:     time.Now().UTC(),
		Playback: snapshot,
	}
}

func isLossyPlaybackEvent(eventType string) bool {
	return eventType == "chunk.started" || eventType == "progress.updated"
}
