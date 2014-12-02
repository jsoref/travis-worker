package lib

import (
	"reflect"
	"testing"
)

type testMessageProcessor struct {
	t *testing.T
}

func (p *testMessageProcessor) Process(payload Payload) {
	want := Payload{
		Job: JobPayload{
			ID:     12345,
			Number: "1",
		},
	}

	if !reflect.DeepEqual(payload, want) {
		p.t.Errorf("got payload %+v, want %+v", payload, want)
	}

	return
}

func TestQueue(t *testing.T) {
	mb := NewTestMessageBroker()
	mb.DeclareQueue("builds.linux")

	q := NewQueue(mb, "builds.linux", 1)

	mb.Publish("", "builds.linux", "test", []byte(`{"job":{"id":12345,"number":"1"}}`))
	mb.Close()

	q.Subscribe(nil, func() JobPayloadProcessor {
		return &testMessageProcessor{t}
	})
}