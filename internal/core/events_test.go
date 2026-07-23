package core

import (
	"testing"
	"time"
)

func TestEventBrokerAssignsSequenceNumbers(t *testing.T) {
	broker := NewEventBroker()
	events, unsubscribe := broker.Subscribe()
	defer unsubscribe()

	broker.Publish(PlaybackEvent{Type: "playback.started"})
	first := receiveEvent(t, events)
	if first.Seq != 1 {
		t.Fatalf("очікував seq=1, отримав %#v", first)
	}
	if first.Time.IsZero() {
		t.Fatalf("очікував заповнений timestamp, отримав %#v", first)
	}

	second := broker.NewEvent("playback.snapshot", PlaybackSnapshot{State: playbackPlaying})
	if second.Seq != 2 || second.Type != "playback.snapshot" {
		t.Fatalf("неочікуваний snapshot event: %#v", second)
	}
}

func TestEventBrokerDropsLossyProgressForSlowClient(t *testing.T) {
	broker := NewEventBroker()
	events, unsubscribe := broker.Subscribe()
	defer unsubscribe()

	for i := 0; i < eventBufferSize+5; i++ {
		broker.Publish(PlaybackEvent{Type: "progress.updated"})
	}

	for i := 0; i < eventBufferSize; i++ {
		event := receiveEvent(t, events)
		if event.Type != "progress.updated" {
			t.Fatalf("очікував progress.updated, отримав %#v", event)
		}
	}

	select {
	case event, ok := <-events:
		t.Fatalf("lossy progress не має закривати або переповнювати канал, ok=%v event=%#v", ok, event)
	default:
	}
}

func TestEventBrokerDisconnectsSlowClientOnReliableEvent(t *testing.T) {
	broker := NewEventBroker()
	events, unsubscribe := broker.Subscribe()
	defer unsubscribe()

	for i := 0; i < eventBufferSize; i++ {
		broker.Publish(PlaybackEvent{Type: "progress.updated"})
	}
	broker.Publish(PlaybackEvent{Type: "playback.finished"})

	for i := 0; i < eventBufferSize; i++ {
		event := receiveEvent(t, events)
		if event.Type != "progress.updated" {
			t.Fatalf("очікував buffered progress.updated, отримав %#v", event)
		}
	}

	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("очікував закритий канал для повільного клієнта")
		}
	case <-time.After(time.Second):
		t.Fatal("reliable event не відключив повільного клієнта")
	}
}

func TestEventBrokerDeliversReliableEventWhenClientHasCapacity(t *testing.T) {
	broker := NewEventBroker()
	events, unsubscribe := broker.Subscribe()
	defer unsubscribe()

	broker.Publish(PlaybackEvent{Type: "playback.failed"})
	event := receiveEvent(t, events)
	if event.Type != "playback.failed" {
		t.Fatalf("очікував playback.failed, отримав %#v", event)
	}
}

func receiveEvent(t *testing.T, events <-chan PlaybackEvent) PlaybackEvent {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("канал подій закритий")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("подія не надійшла")
		return PlaybackEvent{}
	}
}
