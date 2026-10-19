package sequencer

import (
	"context"
	"fmt"
	"testing"

	"github.com/damione1/Homeward/internal/domain"
	"github.com/damione1/Homeward/internal/integrations"
)

type nha184BenchmarkMode uint8

const (
	nha184BenchmarkEmpty nha184BenchmarkMode = iota
	nha184BenchmarkSaturated
	nha184BenchmarkFullRead
	nha184BenchmarkDrained
)

func BenchmarkNHA184RecorderEmpty(b *testing.B) {
	nha184BenchmarkRecorder(b, nha184BenchmarkEmpty)
}

func BenchmarkNHA184RecorderSaturated(b *testing.B) {
	nha184BenchmarkRecorder(b, nha184BenchmarkSaturated)
}

func BenchmarkNHA184RecorderFullRead(b *testing.B) {
	nha184BenchmarkRecorder(b, nha184BenchmarkFullRead)
}

func BenchmarkNHA184RecorderDrained(b *testing.B) {
	nha184BenchmarkRecorder(b, nha184BenchmarkDrained)
}

// This file deliberately uses no queue representation. Keep its bytes identical
// for the measured parent and the replacement. Only the actual mailbox paths
// (publish under the owner lock, and Next) belong to the timed operation.
func nha184BenchmarkRecorder(b *testing.B, mode nha184BenchmarkMode) {
	for _, capacity := range []int{16, 64, 256, 1024} {
		b.Run(fmt.Sprintf("capacity=%d", capacity), func(b *testing.B) {
			for _, payload := range []int{64, 256} {
				b.Run(fmt.Sprintf("payload=%d", payload), func(b *testing.B) {
					nha184BenchmarkCase(b, mode, capacity, payload)
				})
			}
		})
	}
}

func nha184BenchmarkCase(b *testing.B, mode nha184BenchmarkMode, capacity, payload int) {
	limits, err := NewQueueLimits(1, 1, 1, capacity)
	if err != nil {
		b.Fatal(err)
	}
	s, err := NewWithLimits(RealClock{}, 1, limits)
	if err != nil {
		b.Fatal(err)
	}
	start, err := s.Start()
	if err != nil {
		b.Fatal(err)
	}
	instance, err := domain.ParseIntegrationInstanceID("nha184-benchmark")
	if err != nil {
		b.Fatal(err)
	}
	sourceSequence := uint64(1)
	source, err := domain.NewSourceStamp("nha184-benchmark-session", &sourceSequence)
	if err != nil {
		b.Fatal(err)
	}
	data := make([]byte, payload)
	for i := range data {
		data[i] = byte(i)
	}
	edge, err := integrations.NewNativeMQTTEdge("nha184/bench", data, false, sourceSequence)
	if err != nil {
		b.Fatal(err)
	}
	if err := s.AcceptNativeMQTTEdge(context.Background(), instance, source, edge); err != nil {
		b.Fatal(err)
	}
	entries, _, err := s.ReadAfter(start, 1)
	if err != nil || len(entries) != 1 {
		b.Fatalf("admitted template: entries=%d err=%v", len(entries), err)
	}
	entry := entries[0]
	if entry.EstimatedBytes() <= 0 || entry.EstimatedBytes() > MaxRecorderQueueBytes/capacity {
		b.Fatalf("capacity does not fit the unchanged byte quota: capacity=%d charge=%d quota=%d", capacity, entry.EstimatedBytes(), MaxRecorderQueueBytes)
	}
	sub, _, err := s.SubscribeRecorder()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := sub.Close(); err != nil {
			b.Error(err)
		}
	})
	// Next must always have an available delivery in this workload. A canceled
	// context bounds a broken empty-queue path without timing context creation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	publish := func() {
		s.mu.Lock()
		entry.cursor.seq++
		s.publish(entry)
		s.mu.Unlock()
	}
	read := func() {
		delivery, err := sub.Next(ctx)
		if err != nil || delivery.Kind() != DeliveryEntry {
			b.Fatalf("expected available entry: kind=%d err=%v", delivery.Kind(), err)
		}
	}
	initialLost := uint64(0)
	wantQueued := 0
	switch mode {
	case nha184BenchmarkEmpty:
	case nha184BenchmarkSaturated, nha184BenchmarkFullRead:
		for i := 0; i < capacity; i++ {
			publish()
		}
		wantQueued = capacity
	case nha184BenchmarkDrained:
		for i := 0; i < 2*capacity+3; i++ {
			publish()
		}
		initialLost = uint64(capacity + 3)
		delivery, err := sub.Next(ctx)
		loss, ok := delivery.Loss()
		if err != ErrRecorderGap || !ok || loss.Count != initialLost {
			b.Fatalf("drained history loss: %+v %v", delivery, err)
		}
		for i := 0; i < capacity; i++ {
			read()
		}
	default:
		b.Fatal("invalid benchmark mode")
	}
	status, err := sub.Status()
	if err != nil || status.Queued != wantQueued || status.QueuedBytes != wantQueued*entry.EstimatedBytes() || status.PendingLoss != nil || status.Lost != initialLost {
		b.Fatalf("setup: %+v err=%v", status, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		switch mode {
		case nha184BenchmarkEmpty, nha184BenchmarkDrained:
			publish()
			read()
		case nha184BenchmarkSaturated:
			publish()
		case nha184BenchmarkFullRead:
			read()
			publish()
		}
	}
	b.StopTimer()
	status, err = sub.Status()
	wantLost := initialLost
	if mode == nha184BenchmarkSaturated {
		wantLost += uint64(b.N)
	}
	if err != nil || status.Queued != wantQueued || status.QueuedBytes != wantQueued*entry.EstimatedBytes() || status.Lost != wantLost {
		b.Fatalf("final occupation/accounting: %+v err=%v", status, err)
	}
	if mode == nha184BenchmarkSaturated {
		if status.PendingLoss == nil || status.PendingLoss.Count != uint64(b.N) || status.ReplayValid {
			b.Fatalf("final saturation loss: %+v", status)
		}
	} else if status.PendingLoss != nil || status.ReplayValid != (initialLost == 0) {
		b.Fatalf("final replay state: %+v", status)
	}
}
