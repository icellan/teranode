package kafka

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newInMemoryTestProducer(t *testing.T) *KafkaAsyncProducer {
	t.Helper()

	kafkaURL, err := url.Parse("memory://localhost/stop-race-topic")
	require.NoError(t, err)

	producer, err := NewKafkaAsyncProducerFromURL(context.Background(), &mockAsyncLogger{}, kafkaURL, nil)
	require.NoError(t, err)

	return producer
}

// TestKafkaAsyncProducer_PublishLoopUsesTheChannelItWasGiven pins the invariant behind a
// 10-minute CI hang in services/blockvalidation.
//
// The publish loop used to re-read c.publishChannel instead of ranging over the channel it was
// handed. Stop() nils that field under the write lock, so a Stop that won the race left the loop
// ranging over a nil channel — which blocks forever, so the deferred publishWg.Done() never ran
// and Stop() parked in publishWg.Wait() until the test binary timed out.
//
// This drives the loop directly in exactly the state Stop() leaves behind: field nil, but a live
// (already closed) channel passed in. Ranging over the argument returns; ranging over the field
// hangs.
func TestKafkaAsyncProducer_PublishLoopUsesTheChannelItWasGiven(t *testing.T) {
	producer := newInMemoryTestProducer(t)

	producer.channelMu.Lock()
	producer.publishChannel = nil
	producer.channelMu.Unlock()

	ch := make(chan *Message, 1)
	close(ch)

	done := make(chan struct{})

	go func() {
		defer close(done)

		producer.runInMemoryPublishLoop(ch)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish loop never returned: it is ranging over c.publishChannel (nil) instead of the channel it was given")
	}
}

// TestKafkaAsyncProducer_ConcurrentStartStopDoesNotHang covers the other half of the same
// lifecycle bug: a Stop() that reaches the channel field before Start() has stored it sees nil,
// closes nothing, and then waits on a publish goroutine whose channel nobody will ever close.
// Start and Stop are serialised so neither ordering can wedge shutdown.
func TestKafkaAsyncProducer_ConcurrentStartStopDoesNotHang(t *testing.T) {
	for i := 0; i < 200; i++ {
		producer := newInMemoryTestProducer(t)

		var wg sync.WaitGroup

		wg.Add(2)

		go func() {
			defer wg.Done()

			producer.Start(context.Background(), make(chan *Message, 1))
		}()

		go func() {
			defer wg.Done()

			_ = producer.Stop()
		}()

		settled := make(chan struct{})

		go func() {
			defer close(settled)

			wg.Wait()
		}()

		select {
		case <-settled:
		case <-time.After(10 * time.Second):
			t.Fatalf("Start/Stop deadlocked on iteration %d", i)
		}

		// Whichever order they landed in, a follow-up Stop must still return.
		stopped := make(chan struct{})

		go func() {
			defer close(stopped)

			_ = producer.Stop()
		}()

		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Fatalf("second Stop() hung on iteration %d", i)
		}
	}
}
