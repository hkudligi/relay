package core

import (
	"errors"
	"testing"
)

func TestIsModelSelectionError(t *testing.T) {
	for _, message := range []string{
		"model is unavailable",
		"unknown model claude-opus",
		"unsupported model",
		"failed to initialize model",
	} {
		if !IsModelSelectionError(errors.New(message)) {
			t.Errorf("IsModelSelectionError(%q) = false", message)
		}
	}
	if IsModelSelectionError(errors.New("syntax error in generated code")) {
		t.Fatal("task errors must not be classified as model selection errors")
	}
}
