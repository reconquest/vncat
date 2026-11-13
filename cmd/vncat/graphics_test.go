package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"strings"
	"testing"
)

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestGraphicsPNGChunksAndAspect(t *testing.T) {
	frame := image.NewRGBA(image.Rect(0, 0, 320, 160))
	random := rand.New(rand.NewSource(1))
	for y := 0; y < 160; y++ {
		for x := 0; x < 320; x++ {
			frame.SetRGBA(x, y, color.RGBA{uint8(random.Intn(256)), uint8(random.Intn(256)), uint8(random.Intn(256)), 255})
		}
	}
	var output bytes.Buffer
	view, err := render(&output, frame, terminalSize{columns: 40, rows: 15, cellWidth: 8, cellHeight: 16})
	if err != nil {
		t.Fatal(err)
	}
	if view.width != 320 || view.height != 160 {
		t.Fatal(view)
	}
	if !strings.Contains(output.String(), "a=T") || !strings.Contains(output.String(), "C=1") || strings.Contains(output.String(), ",c=") || strings.Contains(output.String(), ",r=") {
		t.Fatal("not native pixel placement")
	}
	var encoded strings.Builder
	chunks := 0
	for _, part := range strings.Split(output.String(), "\x1b_G")[1:] {
		command := strings.SplitN(part, "\x1b\\", 2)[0]
		pieces := strings.SplitN(command, ";", 2)
		if len(pieces) != 2 {
			continue
		}
		if len(pieces[1]) > 4096 || len(pieces[1])%4 != 0 {
			t.Fatal("invalid chunk size")
		}
		encoded.WriteString(pieces[1])
		chunks++
	}
	if chunks < 2 || !strings.Contains(output.String(), "m=1") || !strings.Contains(output.String(), "m=0") {
		t.Fatal("not chunked")
	}
	data, err := base64.StdEncoding.DecodeString(encoded.String())
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds() != frame.Rect || decoded.At(8, 9) != frame.At(8, 9) {
		t.Fatal("pixel payload mismatch")
	}
	output.Reset()
	frame.SetRGBA(8, 9, color.RGBA{255, 0, 0, 255})
	if _, err := render(&output, frame, terminalSize{columns: 10, rows: 20, cellWidth: 8, cellHeight: 16}); err != nil {
		t.Fatal(err)
	}
	if _, err := render(failedWriter{}, frame, terminalSize{columns: 1, rows: 1, cellWidth: 8, cellHeight: 16}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

func TestFitAndPointerGeometry(t *testing.T) {
	for _, tc := range []struct {
		frame         image.Rectangle
		size          terminalSize
		width, height int
	}{
		{image.Rect(0, 0, 1920, 1080), terminalSize{80, 24, 10, 20}, 800, 450},
		{image.Rect(0, 0, 800, 1200), terminalSize{80, 24, 10, 20}, 320, 480},
		{image.Rect(0, 0, 2, 1), terminalSize{8, 2, 10, 20}, 80, 40},
	} {
		view, err := fit(tc.frame, tc.size)
		if err != nil || view.width != tc.width || view.height != tc.height {
			t.Fatal(view, err)
		}
		x, y, inside := view.point(0, 0)
		if !inside || int(x) != tc.size.cellWidth/2*tc.frame.Dx()/tc.width || int(y) != tc.size.cellHeight/2*tc.frame.Dy()/tc.height {
			t.Fatal(x, y, inside)
		}
	}
	for _, size := range []terminalSize{{}, {80, 24, 0, 0}, {65535, 65535, 1000, 1000}} {
		if _, err := fit(image.Rect(0, 0, 1, 1), size); err == nil {
			t.Fatal("accepted invalid metrics")
		}
	}
}

func TestCLI(t *testing.T) {
	for _, args := range [][]string{{"host:5900"}, {"-R", "host:5900"}, {"host:5900", "-R"}, {"[::1]:5900", "-R"}, {"-R", "[2001:db8::1]:5901"}, {":5900"}} {
		got, err := parseArgs(args)
		if err != nil {
			t.Fatal(args, err)
		}
		if got.readonly != (len(args) > 1) {
			t.Fatal(args, got)
		}
	}
	for _, args := range [][]string{nil, {"host"}, {"host:0"}, {"host:65536"}, {"host:http"}, {"::1:5900"}, {"host:5900", "other:5900"}, {"host:5900", "-unknown"}, {"-R"}} {
		if _, err := parseArgs(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	for _, arg := range []string{"-h", "--help"} {
		got, err := parseArgs([]string{arg})
		if err != nil || !got.help {
			t.Fatal(got, err)
		}
	}
}
