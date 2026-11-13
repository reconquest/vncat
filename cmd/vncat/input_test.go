package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func decodeInput(t *testing.T, sequence string) []uv.Event {
	t.Helper()
	var decoder eventDecoder
	events, err := decoder.feed([]byte(sequence), true)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func keyMessages(t *testing.T, data []byte) []uint32 {
	t.Helper()
	var result []uint32
	for len(data) > 0 {
		if len(data) < 8 || data[0] != 4 {
			t.Fatalf("not key messages: %x", data)
		}
		value := wireOrder.Uint32(data[4:8])
		if data[1] != 0 {
			value |= 0x80000000
		}
		result = append(result, value)
		data = data[8:]
	}
	return result
}

func TestLegacyKeys(t *testing.T) {
	cases := []struct {
		input    string
		symbol   uint32
		modifier uint32
	}{
		{"a", 'a', 0}, {"A", 'A', 0xffe1}, {"é", 233, 0}, {"界", 0x0100754c, 0}, {"🙂", 0x0101f642, 0},
		{"\x03", 'c', 0xffe3}, {"\x1bx", 'x', 0xffe9},
		{"\x1b[A", 0xff52, 0}, {"\x1b[3~", 0xffff, 0}, {"\x1b[5~", 0xff55, 0},
		{"\x1b[H", 0xff50, 0}, {"\x1b[F", 0xff57, 0}, {"\x1bOP", 0xffbe, 0},
		{"\x1b[24~", 0xffc9, 0}, {"\x1b[1;5D", 0xff51, 0xffe3},
		{"\t", 0xff09, 0}, {"\r", 0xff0d, 0}, {"\x7f", 0xff08, 0}, {"\x1b", 0xff1b, 0},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			conn := newMemoryConn(nil)
			state := inputState{client: &rfbClient{conn: conn}}
			for _, event := range decodeInput(t, tc.input) {
				quit, err := state.handle(event)
				if quit || err != nil {
					t.Fatal(quit, err)
				}
			}
			want := []uint32{tc.symbol | 0x80000000, tc.symbol}
			if tc.modifier != 0 {
				want = append([]uint32{tc.modifier | 0x80000000}, want...)
				want = append(want, tc.modifier)
			}
			got := keyMessages(t, conn.bytes())
			if len(got) != len(want) {
				t.Fatalf("got %x want %x", got, want)
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("got %x want %x", got, want)
				}
			}
		})
	}
}

func TestKittyHeldKeysAndQuit(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}}
	sequence := "\x1b[?15u\x1b[97;5:1u\x1b[97;5:2u\x1b[97;5:3u\x1b[99;5u\x1b[93;5u"
	quit := false
	for _, event := range decodeInput(t, sequence) {
		var err error
		quit, err = state.handle(event)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !quit || !state.kitty {
		t.Fatal("quit or Kitty negotiation")
	}
	if err := state.release(); err != nil {
		t.Fatal(err)
	}
	got := keyMessages(t, conn.bytes())
	want := []uint32{0x8000ffe3, 0x80000061, 0x80000061, 0x61, 0x80000063, 0x63, 0xffe3}
	if len(got) != len(want) {
		t.Fatalf("got %x want %x", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %x want %x", got, want)
		}
	}
	before := len(conn.bytes())
	if err := state.release(); err != nil {
		t.Fatal(err)
	}
	if len(conn.bytes()) != before {
		t.Fatal("cleanup not idempotent")
	}
}

func TestKittyShiftReleaseIdentity(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}, kitty: true}
	for _, sequence := range []string{"\x1b[97:65;2u", "\x1b[57441;1:3u", "\x1b[97;1:3u"} {
		for _, event := range decodeInput(t, sequence) {
			if _, err := state.handle(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	got := keyMessages(t, conn.bytes())
	want := []uint32{0x8000ffe1, 0x80000041, 0xffe1, 0x41}
	if len(got) != len(want) {
		t.Fatalf("got %x", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %x want %x", got, want)
		}
	}
}

func TestInputFragmentationAndBounds(t *testing.T) {
	sequence := "界\x1b[1;5A\x1b[97;1:3u\x1b[<0;4;5M\x1b_Gi=31;OK\x1b\\"
	for split := 0; split <= len(sequence); split++ {
		var decoder eventDecoder
		first, err := decoder.feed([]byte(sequence[:split]), false)
		if err != nil {
			t.Fatal(err)
		}
		last, err := decoder.feed([]byte(sequence[split:]), false)
		if err != nil {
			t.Fatal(err)
		}
		if len(first)+len(last) != 5 || len(decoder.pending) != 0 {
			t.Fatalf("split %d: %v %v %q", split, first, last, decoder.pending)
		}
	}
	var decoder eventDecoder
	if _, err := decoder.feed([]byte("\x1b["+strings.Repeat("1", 5000)), false); err == nil {
		t.Fatal("unbounded input")
	}
	decoder = eventDecoder{}
	events, err := decoder.feed([]byte("\x1b[123"), true)
	if err != nil || len(events) != 0 || len(decoder.pending) != 0 {
		t.Fatal(events, err)
	}
}

func TestMouseMappingDragReleaseWheel(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}, view: viewport{width: 100, height: 50, remoteWidth: 1000, remoteHeight: 500, cellWidth: 10, cellHeight: 10}}
	sequence := "\x1b[<0;3;2M\x1b[<32;5;3M\x1b[<0;20;20m\x1b[<64;2;2M\x1b[<65;2;2M\x1b[<2;1;1M"
	for _, event := range decodeInput(t, sequence) {
		if _, err := state.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.release(); err != nil {
		t.Fatal(err)
	}
	data := conn.bytes()
	masks := []byte{1, 1, 0, 8, 0, 16, 0, 4, 0}
	if len(data) != len(masks)*6 {
		t.Fatalf("messages %x", data)
	}
	for i, mask := range masks {
		if data[i*6] != 5 || data[i*6+1] != mask {
			t.Fatalf("mask %d: %x", i, data)
		}
	}
	if wireOrder.Uint16(data[2:]) != 250 || wireOrder.Uint16(data[4:]) != 150 {
		t.Fatal("click mapping")
	}
	if wireOrder.Uint16(data[8:]) != 450 || wireOrder.Uint16(data[10:]) != 250 {
		t.Fatal("drag mapping")
	}
	if wireOrder.Uint16(data[14:]) != 999 || wireOrder.Uint16(data[16:]) != 499 {
		t.Fatal("release clamp")
	}
	count := len(data)
	for _, event := range decodeInput(t, "\x1b[<0;20;20M") {
		if _, err := state.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if len(conn.bytes()) != count {
		t.Fatal("outside click forwarded")
	}
}

func TestReadonlyIsolation(t *testing.T) {
	conn := newMemoryConn(nil)
	client := &rfbClient{conn: conn, readonly: true}
	state := inputState{client: client, kitty: true, held: map[rune]uint32{'x': 'x'}, modifiers: uv.ModCtrl, buttons: 1, view: viewport{width: 100, height: 100, remoteWidth: 100, remoteHeight: 100, cellWidth: 10, cellHeight: 10}}
	for _, event := range decodeInput(t, "a\x03\x1b[97;1:3u\x1b[<0;1;1M\x1b[<32;2;2M\x1b[<0;2;2m\x1b[<64;1;1M") {
		if _, err := state.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.key('x', true); err != nil {
		t.Fatal(err)
	}
	if err := client.pointer(1, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := state.release(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(conn.bytes(), nil) {
		t.Fatalf("readonly sent %x", conn.bytes())
	}
	events := decodeInput(t, "\x1d")
	quit, err := state.handle(events[0])
	if !quit || err != nil {
		t.Fatal("readonly cannot quit", err)
	}
}
func TestLegacyTextWithKittyEnabled(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}, kitty: true}
	for _, event := range decodeInput(t, "a界\x03") {
		if _, err := state.handle(event); err != nil {
			t.Fatal(err)
		}
	}
	if len(state.held) != 0 || state.modifiers != 0 {
		t.Fatal("legacy text left held input")
	}
	got := keyMessages(t, conn.bytes())
	want := []uint32{0x80000061, 0x61, 0x8100754c, 0x0100754c, 0x8000ffe3, 0x80000063, 0x63, 0xffe3}
	if len(got) != len(want) {
		t.Fatalf("got %x", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %x want %x", got, want)
		}
	}
}

func TestHeldInputFailureAndLimit(t *testing.T) {
	conn := newMemoryConn(nil)
	state := inputState{client: &rfbClient{conn: conn}, kitty: true}
	if _, err := state.handle(uv.KeyPressEvent{Code: 'a', Mod: uv.ModCtrl}); err != nil {
		t.Fatal(err)
	}
	conn.failure = io.ErrClosedPipe
	if _, err := state.handle(uv.KeyReleaseEvent{Code: 'a', Mod: uv.ModCtrl}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if state.held['a'] != 'a' {
		t.Fatal("failed release forgot held key")
	}
	conn.failure = nil
	if err := state.release(); err != nil {
		t.Fatal(err)
	}
	if len(state.held) != 0 || state.modifiers != 0 {
		t.Fatal("cleanup state")
	}
	state.held = make(map[rune]uint32)
	for i := 0; i < 256; i++ {
		state.held[rune(i+32)] = uint32(i + 32)
	}
	if _, err := state.handle(uv.KeyPressEvent{Code: 'a'}); err != nil {
		t.Fatal("repeat at limit", err)
	}
	if _, err := state.handle(uv.KeyPressEvent{Code: '界'}); err == nil {
		t.Fatal("unbounded held keys")
	}
}

func TestPartialCellHit(t *testing.T) {
	view := viewport{width: 12, height: 12, remoteWidth: 120, remoteHeight: 120, cellWidth: 10, cellHeight: 10}
	x, y, inside := view.point(1, 1)
	if !inside || x != 119 || y != 119 {
		t.Fatal(x, y, inside)
	}
	if _, _, inside := view.point(2, 1); inside {
		t.Fatal("outside cell accepted")
	}
}

func FuzzTerminalDecoder(f *testing.F) {
	for _, sequence := range []string{"a界", "\x1b[97;5:3u", "\x1b[<0;2;3M", "\x1b_Gi=31;OK\x1b\\", "\x1b[999999999999999999999999u"} {
		f.Add([]byte(sequence))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			t.Skip()
		}
		var decoder eventDecoder
		for len(data) > 0 {
			size := min(17, len(data))
			if _, err := decoder.feed(data[:size], false); err != nil {
				return
			}
			data = data[size:]
		}
		if _, err := decoder.feed(nil, true); err != nil {
			return
		}
		if len(decoder.pending) != 0 {
			t.Fatal("pending data after timeout")
		}
	})
}
