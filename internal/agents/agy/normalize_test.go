package agy

import (
	"testing"

	"github.com/harsha/relay/internal/agents"
)

func TestNormalizeReportsProviderErrorAndModel(t *testing.T) {
	modelEvent, ok, err := normalize([]byte(`{"event":"init","conversation_id":"conv-123","init":{"model":"claude-opus-4-6-thinking"}}`))
	if err != nil || !ok {
		t.Fatalf("init event = %+v, ok=%v, err=%v", modelEvent, ok, err)
	}
	if got := modelEvent.Data["model"]; got != "claude-opus-4-6-thinking" {
		t.Fatalf("reported model = %v", got)
	}
	errorEvent, ok, err := normalize([]byte(`{"event":"step_update","step_update":{"step_type":"error_message","state":"DONE","error_message":"model is unavailable"}}`))
	if err != nil || !ok {
		t.Fatalf("error event = %+v, ok=%v, err=%v", errorEvent, ok, err)
	}
	if errorEvent.Kind != agents.EventError || errorEvent.Message != "model is unavailable" {
		t.Fatalf("error event = %+v", errorEvent)
	}
	topLevelError, ok, err := normalize([]byte(`{"event":"step_update","error_message":"model rejected the request","step_update":{"step_type":"error_message","state":"DONE"}}`))
	if err != nil || !ok || topLevelError.Message != "model rejected the request" {
		t.Fatalf("top-level error event = %+v, ok=%v, err=%v", topLevelError, ok, err)
	}
}

func TestNormalizeTreatsFailedStepStateAsProviderError(t *testing.T) {
	event, ok, err := normalize([]byte(`{"event":"step_update","step_update":{"step_type":"tool","state":"ERROR","message":"model at capacity"}}`))
	if err != nil || !ok {
		t.Fatalf("event = %+v, ok=%v, err=%v", event, ok, err)
	}
	if event.Kind != agents.EventError || event.Message != "model at capacity" {
		t.Fatalf("event = %+v", event)
	}
}
