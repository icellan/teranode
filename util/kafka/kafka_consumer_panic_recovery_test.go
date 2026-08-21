package kafka

import (
	"context"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestConsumerRecoversFromPanickingHandler proves a handler panic on the
// in-memory Kafka path does not take down the process (there is no recover()
// anywhere else in this package - see AGENTS-tracked defect on
// validatortxs) and that the consumer keeps making progress afterwards
// instead of wedging forever on the poison message.
func TestConsumerRecoversFromPanickingHandler(t *testing.T) {
	topic := "panic-recovery-topic"

	kafkaURL, err := url.Parse("memory://localhost/" + topic)
	require.NoError(t, err)

	logger := &mockLogger{}
	consumer, err := NewKafkaConsumerGroup(KafkaConsumerConfig{
		Logger:            logger,
		URL:               kafkaURL,
		Topic:             topic,
		ConsumerGroupID:   "panic-recovery-group",
		AutoCommitEnabled: true,
	})
	require.NoError(t, err)

	producer, err := NewKafkaAsyncProducer(logger, KafkaProducerConfig{
		Logger:     logger,
		URL:        kafkaURL,
		Topic:      topic,
		BrokersURL: []string{"localhost"},
	})
	require.NoError(t, err)

	producerCtx, producerCancel := context.WithCancel(context.Background())
	defer producerCancel()
	producer.Start(producerCtx, make(chan *Message, 8))

	var goodMessagesHandled atomic.Int32

	handler := func(msg *KafkaMessage) error {
		if string(msg.Key) == "poison" {
			panic("simulated handler panic on malformed message")
		}

		goodMessagesHandled.Add(1)

		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consumer.Start(ctx, handler)

	// Poison message first: if the panic is not recovered, the test binary
	// itself crashes here and no assertion below ever runs.
	producer.Publish(&Message{Key: []byte("poison"), Value: []byte("boom")})

	// A well-formed message published after the poison one must still be
	// processed - proves the consumer goroutine is alive and not wedged
	// retrying the poison message forever.
	producer.Publish(&Message{Key: []byte("good"), Value: []byte("ok")})

	require.Eventually(t, func() bool {
		return goodMessagesHandled.Load() == 1
	}, 5*time.Second, 10*time.Millisecond, "consumer did not process the message following the panic - it is wedged or dead")

	_ = consumer.Close()
	_ = producer.Stop()
}
