package main

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func TestCoalescedControlStrings(t *testing.T) {
	osc := "\x1b]10;rgb:ffff/0000/0000\a"
	graphics := "\x1b_Gi=31;OK\x1b\\"

	for _, prefix := range []string{osc, "\x1b"} {
		sequence := prefix + graphics + "z"
		want := append(decodeInput(t, prefix), decodeInput(t, graphics+"z")...)

		for split := 0; split <= len(sequence); split++ {
			var decoder eventDecoder

			events, err := decoder.feed([]byte(sequence[:split]), false)
			if err != nil {
				t.Fatal(err)
			}

			rest, err := decoder.feed([]byte(sequence[split:]), false)
			if err != nil {
				t.Fatal(err)
			}

			events = append(events, rest...)
			if !reflect.DeepEqual(events, want) || len(decoder.pending) != 0 {
				t.Fatalf("split %d: got %#v want %#v", split, events, want)
			}

			reply, ok := events[1].(uv.KittyGraphicsEvent)
			if !ok || string(reply.Payload) != "OK" {
				t.Fatal("lost graphics reply", events)
			}
		}
	}

	for _, sequence := range []string{osc + graphics, "\x1b]10;x\x1b\\z\a"} {
		expected := strings.IndexAny(sequence, "\a") + 1
		if strings.Contains(sequence, "x\x1b\\") {
			expected = len("\x1b]10;x\x1b\\")
		}

		if got := sequenceLength([]byte(sequence)); got != expected {
			t.Fatal("wrong string boundary", got, expected)
		}
	}
}

func TestAltPrefixedKeys(t *testing.T) {
	for _, sequence := range []string{"\x1b\x1b[A", "\x1b\x1bOA"} {
		for split := 0; split <= len(sequence); split++ {
			var decoder eventDecoder

			events, err := decoder.feed([]byte(sequence[:split]), false)
			if err != nil {
				t.Fatal(err)
			}

			rest, err := decoder.feed([]byte(sequence[split:]), false)
			if err != nil {
				t.Fatal(err)
			}

			events = append(events, rest...)
			if len(events) != 1 {
				t.Fatalf("split %d: %#v", split, events)
			}

			key, ok := events[0].(legacyKeyEvent)
			if !ok || key.Code != uv.KeyUp || key.Mod != uv.ModAlt {
				t.Fatal("lost Alt-Up", events)
			}

			conn := newMemoryConn(nil)
			state := inputState{client: &rfbClient{conn: conn}, kitty: true}
			requireInput(t, &state, events[0])

			want := []uint32{0x8000ffe9, 0x8000ff52, 0xff52, 0xffe9}
			if got := keyMessages(t, conn.bytes()); !slices.Equal(got, want) {
				t.Fatalf("remote keys: %x", got)
			}
		}
	}
}

func TestEscapeTimeoutAndBound(t *testing.T) {
	for _, sequence := range []string{"\x1b", "\x1b\x1b"} {
		var decoder eventDecoder

		events, err := decoder.feed([]byte(sequence), false)

		if err != nil || len(events) != 0 {
			t.Fatal(events, err)
		}

		events, err = decoder.feed(nil, true)
		if err != nil || len(events) != 1 || len(decoder.pending) != 0 {
			t.Fatal(events, err)
		}

		key, ok := events[0].(legacyKeyEvent)
		expected := uv.KeyMod(0)

		if len(sequence) == 2 {
			expected = uv.ModAlt
		}

		if !ok || key.Code != uv.KeyEscape || key.Mod != expected {
			t.Fatal("wrong timeout key", events)
		}
	}

	var decoder eventDecoder

	_, err := decoder.feed(bytes.Repeat([]byte{27}, sequenceLimit+1), false)
	if err == nil {
		t.Fatal("unbounded nested escapes")
	}
}

func TestModifyOtherKeysReleasesWithKitty(t *testing.T) {
	sequence := "\x1b[?15u\x1b[27;5;99~"
	for split := 0; split <= len(sequence); split++ {
		var decoder eventDecoder

		conn := newMemoryConn(nil)
		state := inputState{client: &rfbClient{conn: conn}}

		for _, piece := range []string{sequence[:split], sequence[split:]} {
			events, err := decoder.feed([]byte(piece), false)
			if err != nil {
				t.Fatal(err)
			}

			for _, event := range events {
				requireInput(t, &state, event)
			}
		}

		want := []uint32{0x8000ffe3, 0x80000063, 0x63, 0xffe3}
		if got := keyMessages(t, conn.bytes()); !slices.Equal(got, want) {
			t.Fatalf("split %d: got %x want %x", split, got, want)
		}

		if !state.kitty || len(state.held) != 0 || state.modifiers != 0 {
			t.Fatal("legacy key retained held input or lost Kitty")
		}
	}
}

func TestCompositeLegacyF3WithKitty(t *testing.T) {
	sequence := "\x1b[?15u\x1b[1;5R\x1b[57366;1:1u"
	for split := 0; split <= len(sequence); split++ {
		var decoder eventDecoder

		conn := newMemoryConn(nil)
		state := inputState{client: &rfbClient{conn: conn}}
		reports := 0

		for _, piece := range []string{sequence[:split], sequence[split:]} {
			events, err := decoder.feed([]byte(piece), false)
			if err != nil {
				t.Fatal(err)
			}

			for _, event := range events {
				if position, ok := event.(uv.CursorPositionEvent); ok {
					reports++

					if position.X != 4 || position.Y != 0 {
						t.Fatal("changed cursor report", position)
					}
				}

				requireInput(t, &state, event)
			}
		}

		want := make([]uint32, 0, 6)

		want = append(want, 0x8000ffe3, 0x8000ffc0, 0xffc0, 0xffe3, 0x8000ffc0)

		if got := keyMessages(t, conn.bytes()); !slices.Equal(got, want) {
			t.Fatalf("split %d: got %x want %x", split, got, want)
		}

		if reports != 1 || !state.kitty || len(state.held) != 1 ||
			state.modifiers != 0 {
			t.Fatal("lost cursor report or Kitty held key", reports, state)
		}

		for _, event := range decodeInput(t, "\x1b[57366;1:3u") {
			requireInput(t, &state, event)
		}

		want = append(want, 0xffc0)
		if got := keyMessages(t, conn.bytes()); !slices.Equal(got, want) {
			t.Fatalf("split %d: release got %x want %x", split, got, want)
		}

		if len(state.held) != 0 || len(decoder.pending) != 0 {
			t.Fatal("retained input")
		}
	}
}
