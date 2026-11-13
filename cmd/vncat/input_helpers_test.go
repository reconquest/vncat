package main

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func requireInput(t *testing.T, state *inputState, event uv.Event) {
	t.Helper()

	_, err := state.handle(event)
	if err != nil {
		t.Fatal(err)
	}
}
