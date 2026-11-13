package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	uv "github.com/charmbracelet/ultraviolet"
)

type terminalPair struct{ master, slave *os.File }

func openPTY(t *testing.T) terminalPair {
	t.Helper()

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = master.Close() })

	ioctlSetPointerIntError := unix.IoctlSetPointerInt(
		int(master.Fd()),
		unix.TIOCSPTLCK,
		0,
	)
	if ioctlSetPointerIntError != nil {
		t.Fatal(ioctlSetPointerIntError)
	}

	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}

	slave, err := os.OpenFile(
		fmt.Sprintf("/dev/pts/%d", number),
		os.O_RDWR|unix.O_NOCTTY,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = slave.Close() })

	ioctlSetWinsizeError := unix.IoctlSetWinsize(
		int(slave.Fd()),
		unix.TIOCSWINSZ,
		&unix.Winsize{Col: 20, Row: 5, Xpixel: 160, Ypixel: 80},
	)
	if ioctlSetWinsizeError != nil {
		t.Fatal(ioctlSetWinsizeError)
	}

	return terminalPair{master: master, slave: slave}
}

func TestTerminalRestorationAndOutputFailure(t *testing.T) {
	slave := openPTY(t).slave

	before, setupError := term.GetState(int(slave.Fd()))
	if setupError != nil {
		t.Fatal(setupError)
	}

	for _, fail := range []bool{false, true} {
		var output bytes.Buffer

		writer := io.Writer(&output)

		if fail {
			writer = failedWriter{}
		}

		session, openTerminalError := openTerminal(slave, writer)
		if fail {
			if !errors.Is(openTerminalError, io.ErrClosedPipe) {
				t.Fatal(openTerminalError)
			}
		} else {
			if openTerminalError != nil {
				t.Fatal(openTerminalError)
			}

			verifyTerminalTransitions(t, session, &output)
		}

		after, openTerminalError := term.GetState(int(slave.Fd()))
		if openTerminalError != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("terminal not restored", openTerminalError)
		}
	}
}

func TestPasswordAndInputCancellation(t *testing.T) {
	pair := openPTY(t)
	master, slave := pair.master, pair.slave

	var output bytes.Buffer

	session, openTerminalError := openTerminal(slave, &output)
	if openTerminalError != nil {
		t.Fatal(openTerminalError)
	}

	defer func() { _ = session.close() }()

	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	passwordDone := make(chan []byte, 1)

	go func() {
		secret, err := readPassword(ctx, int(slave.Fd()), &output)
		if err != nil {
			t.Error(err)
		}

		passwordDone <- secret
	}()

	_, writeStringError := master.WriteString("sëx\x7fcret\r")
	if writeStringError != nil {
		t.Fatal(writeStringError)
	}

	select {
	case secret := <-passwordDone:
		if string(secret) != "sëcret" {
			t.Fatal("password editing")
		}

	case <-time.After(time.Second):
		t.Fatal("password blocked")
	}

	if strings.Contains(output.String(), "sëcret") {
		t.Fatal("password echoed")
	}

	events := make(chan uv.Event, 4)
	done := make(chan error, 1)

	go func() { done <- streamInput(ctx, int(slave.Fd()), events) }()

	for _, piece := range [][]byte{{0xe7}, {0x95}, {0x8c}, {27, '['}, {'A'}} {
		_, writeError := master.Write(piece)
		if writeError != nil {
			t.Fatal(writeError)
		}
	}

	for range 2 {
		select {
		case <-events:
		case <-time.After(time.Second):
			t.Fatal("fragmented input blocked")
		}
	}

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}

	case <-time.After(time.Second):
		t.Fatal("input reader leaked")
	}
}

func TestCommandProcess(t *testing.T) {
	if os.Getenv("VNCTTY_TEST_PROCESS") != "1" {
		return
	}

	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"vnctty"}, os.Args[i+1:]...)

			main()

			return
		}
	}

	t.Fatal("missing command argument delimiter")
}

type wireRecording struct {
	mutex    sync.Mutex
	messages [][]byte
}

func (r *wireRecording) add(message []byte) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.messages = append(r.messages, bytes.Clone(message))
}

func (r *wireRecording) snapshot() [][]byte {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return append([][]byte(nil), r.messages...)
}

type mockSecurity struct{ password bool }

func mockServer(
	conn net.Conn,
	recording *wireRecording,
	securityMode mockSecurity,
) error {
	defer func() { _ = conn.Close() }()

	deadlineError := conn.SetDeadline(time.Now().Add(8 * time.Second))
	if deadlineError != nil {
		return fmt.Errorf("mock server I/O: %w", deadlineError)
	}

	_, writeStringError := io.WriteString(conn, "RFB 003.008\n")
	if writeStringError != nil {
		return fmt.Errorf("mock server I/O: %w", writeStringError)
	}

	var version [12]byte

	_, handshakeReadError := io.ReadFull(conn, version[:])
	if handshakeReadError != nil {
		return fmt.Errorf("mock server I/O: %w", handshakeReadError)
	}

	if string(version[:]) != "RFB 003.008\n" {
		return errors.New("bad client version")
	}

	security := byte(1)
	if securityMode.password {
		security = 2
	}

	_, handshakeWriteError := conn.Write([]byte{1, security})
	if handshakeWriteError != nil {
		return fmt.Errorf("mock server I/O: %w", handshakeWriteError)
	}

	var selected [1]byte

	_, handshakeReadError = io.ReadFull(conn, selected[:])
	if handshakeReadError != nil {
		return fmt.Errorf("mock server I/O: %w", handshakeReadError)
	}

	if selected[0] != security {
		return errors.New("bad security selection")
	}

	if securityMode.password {
		var challenge [16]byte

		_, writeError := conn.Write(challenge[:])
		if writeError != nil {
			return fmt.Errorf("mock server I/O: %w", writeError)
		}

		_, err := io.ReadFull(conn, challenge[:])
		if err != nil {
			return fmt.Errorf("mock server I/O: %w", err)
		}

		return nil
	}

	_, handshakeWriteError = conn.Write([]byte{0, 0, 0, 0})
	if handshakeWriteError != nil {
		return fmt.Errorf("mock server I/O: %w", handshakeWriteError)
	}

	_, handshakeReadError = io.ReadFull(conn, selected[:])
	if handshakeReadError != nil {
		return fmt.Errorf("mock server I/O: %w", handshakeReadError)
	}

	_, handshakeWriteError = conn.Write(serverInit(2, 2))
	if handshakeWriteError != nil {
		return fmt.Errorf("mock server I/O: %w", handshakeWriteError)
	}

	var setup [44]byte

	_, handshakeReadError = io.ReadFull(conn, setup[:])
	if handshakeReadError != nil {
		return fmt.Errorf("mock server I/O: %w", handshakeReadError)
	}

	if setup[0] != 0 || setup[20] != 2 {
		return errors.New("bad initialization")
	}

	updates := 0

	for {
		var kind [1]byte

		_, readFullError := io.ReadFull(conn, kind[:])
		if readFullError != nil {
			return fmt.Errorf("mock server I/O: %w", readFullError)
		}

		sizes := map[byte]int{3: 9, 4: 7, 5: 5}
		length, ok := sizes[kind[0]]

		if !ok {
			return fmt.Errorf("unexpected message %d", kind[0])
		}

		data := make([]byte, 0, length+1)

		data = data[:cap(data)]

		data[0] = kind[0]

		_, readFullError = io.ReadFull(conn, data[1:])
		if readFullError != nil {
			return fmt.Errorf("mock server I/O: %w", readFullError)
		}

		recording.add(data)

		if kind[0] == 3 && updates < 2 {
			var update []byte

			if updates == 0 {
				update = updateData(
					rectangle(
						0,
						0,
						2,
						2,
						0,
						bytes.Repeat([]byte{0, 0, 255, 0}, 4),
					),
				)
			} else {
				update = updateData(
					rectangle(0, 0, 4, 2, -223, nil),
					rectangle(
						0,
						0,
						4,
						2,
						0,
						bytes.Repeat([]byte{0, 255, 0, 0}, 8),
					),
				)
			}

			_, writeError := conn.Write(append([]byte{0}, update...))
			if writeError != nil {
				return fmt.Errorf("mock server I/O: %w", writeError)
			}

			updates++
		}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("condition timed out")
}

func TestCommandLifecycle(t *testing.T) {
	for _, mode := range []string{
		"quit",
		"readonly",
		"signal",
		"disconnect",
		"password-quit",
		"password-signal",
	} {
		t.Run(mode, func(t *testing.T) {
			password := strings.HasPrefix(mode, "password-")
			pair := openPTY(t)
			master, slave := pair.master, pair.slave

			winsizeError := unix.IoctlSetWinsize(
				int(slave.Fd()),
				unix.TIOCSWINSZ,
				&unix.Winsize{Col: 40, Row: 5, Xpixel: 320, Ypixel: 80},
			)
			if winsizeError != nil {
				t.Fatal(winsizeError)
			}

			original, setupError := term.GetState(int(slave.Fd()))
			if setupError != nil {
				t.Fatal(setupError)
			}

			listener, setupError := (&net.ListenConfig{}).Listen(
				t.Context(),
				"tcp",
				"127.0.0.1:0",
			)
			if setupError != nil {
				t.Fatal(setupError)
			}

			defer func() { _ = listener.Close() }()

			recording := &wireRecording{}
			serverDone := make(chan error, 1)
			connected := make(chan net.Conn, 1)

			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverDone <- err

					return
				}

				connected <- conn

				serverDone <- mockServer(
					conn, recording, mockSecurity{password: password},
				)
			}()

			executable, setupError := os.Executable()
			if setupError != nil {
				t.Fatal(setupError)
			}

			args := []string{
				"-test.run=^TestCommandProcess$",
				"--",
				listener.Addr().String(),
			}
			if mode == "readonly" {
				args = append(args, "-R")
			} else {
				args = append(args, "-X")
			}

			command := exec.CommandContext(t.Context(), executable, args...)

			command.Env = append(os.Environ(), "VNCTTY_TEST_PROCESS=1")
			command.Stdin, command.Stdout, command.Stderr = slave, slave, slave

			startError := command.Start()
			if startError != nil {
				t.Fatal(startError)
			}

			defer func() { _ = command.Process.Kill() }()

			processDone := make(chan error, 1)

			go func() { processDone <- command.Wait() }()

			outputText := recordTerminal(t, master)

			var conn net.Conn

			select {
			case conn = <-connected:
			case <-time.After(3 * time.Second):
				t.Fatal("no VNC connection")
			}

			defer func() { _ = conn.Close() }()

			if password {
				waitFor(
					t,
					func() bool {
						return strings.Contains(outputText(), "VNC password")
					},
				)
			} else {
				waitFor(
					t,
					func() bool {
						return strings.Contains(outputText(), "a=T")
					},
				)

				verifyLiveSession(
					t,
					mode,
					master,
					slave,
					command,
					outputText,
					recording,
				)
			}

			switch mode {
			case "signal", "password-signal":
				setupError = command.Process.Signal(unix.SIGTERM)

			case "disconnect":
				setupError = conn.Close()

			case "password-quit":
				_, setupError = master.WriteString("\x1d")

			default:
				_, setupError = master.WriteString("\x1b[93;5u")
			}

			if setupError != nil {
				t.Fatal(setupError)
			}

			select {
			case err := <-processDone:
				if mode != "disconnect" && err != nil {
					t.Fatalf("process failed: %v %q", err, outputText())
				}

			case <-time.After(5 * time.Second):
				t.Fatal("client did not exit")
			}

			waitFor(
				t,
				func() bool {
					return strings.Contains(outputText(), leaveScreen)
				},
			)

			after, setupError := term.GetState(int(slave.Fd()))
			if setupError != nil || !reflect.DeepEqual(original, after) {
				t.Fatal("process left terminal raw", setupError)
			}

			select {
			case serverError := <-serverDone:
				if serverError != nil && !errors.Is(serverError, io.EOF) &&
					!errors.Is(serverError, net.ErrClosed) &&
					!errors.Is(serverError, io.ErrClosedPipe) {
					t.Fatal(serverError)
				}

			case <-time.After(time.Second):
				t.Fatal("server connection leaked")
			}

			if !password && !strings.Contains(outputText(), leaveInput) {
				t.Fatal("input modes not restored")
			}

			verifyReleasedInput(t, mode, recording)
		})
	}
}

func TestServeFailureCleanup(t *testing.T) {
	for _, mode := range []string{
		"graphics-error",
		"cancel",
		"missing-metrics",
		"resize-missing-metrics",
	} {
		t.Run(mode, func(t *testing.T) {
			pair := openPTY(t)
			master, slave := pair.master, pair.slave

			if mode == "missing-metrics" {
				err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ,
					&unix.Winsize{Col: 20, Row: 5})
				if err != nil {
					t.Fatal(err)
				}
			}

			var output bytes.Buffer

			terminal, openTerminalError := openTerminal(slave, &output)
			if openTerminalError != nil {
				t.Fatal(openTerminalError)
			}

			defer func() { _ = terminal.close() }()

			clientConn, serverConn := net.Pipe()

			defer func() { _ = serverConn.Close() }()

			client := &rfbClient{conn: clientConn}

			resizeError := client.resize(2, 2)
			if resizeError != nil {
				t.Fatal(resizeError)
			}

			peerDone := make(chan struct{})

			go func() {
				_, _ = io.Copy(io.Discard, serverConn)

				close(peerDone)
			}()

			ctx, cancel := context.WithCancel(t.Context())

			defer cancel()

			done := make(chan error, 1)
			resizes := make(chan os.Signal, 1)

			go func() { done <- serve(ctx, client, terminal, resizes) }()

			switch mode {
			case "graphics-error":
				_, openTerminalError = master.WriteString(
					"\x1b_Gi=31;ENOTSUP\x1b\\",
				)

			case "cancel":
				cancel()

			case "missing-metrics", "resize-missing-metrics":
				_, openTerminalError = master.WriteString("\x1b_Gi=31;OK\x1b\\")

				if mode == "resize-missing-metrics" {
					time.Sleep(2100 * time.Millisecond)

					err := unix.IoctlSetWinsize(
						int(slave.Fd()),
						unix.TIOCSWINSZ,
						&unix.Winsize{Col: 40, Row: 10},
					)
					if err != nil {
						t.Fatal(err)
					}

					resizes <- unix.SIGWINCH
				}

			default:
				t.Fatal("unexpected mode", mode)
			}

			if openTerminalError != nil {
				t.Fatal(openTerminalError)
			}

			select {
			case err := <-done:
				switch mode {
				case "cancel":
					if err != nil {
						t.Fatal(err)
					}

				case "graphics-error":
					if err == nil ||
						!strings.Contains(err.Error(), "rejected Kitty") {
						t.Fatal(err)
					}

				case "missing-metrics", "resize-missing-metrics":
					if err == nil ||
						!strings.Contains(err.Error(), "pixel dimensions") {
						t.Fatal(err)
					}

				default:
					t.Fatal("unexpected mode", mode)
				}

			case <-time.After(4 * time.Second):
				t.Fatal("serve failed to stop")
			}

			select {
			case <-peerDone:
			case <-time.After(time.Second):
				t.Fatal("connection leaked")
			}
		})
	}
}

func TestReadSizeInvalidatesMissingPixels(t *testing.T) {
	slave := openPTY(t).slave
	size, setupError := readSize(int(slave.Fd()), terminalSize{})

	if setupError != nil || size.cellWidth != 8 || size.cellHeight != 16 {
		t.Fatal(size, setupError)
	}

	for _, winsize := range []unix.Winsize{
		{Col: 20, Row: 5},
		{Col: 40, Row: 10},
		{Col: 40, Row: 10, Xpixel: 20, Ypixel: 5},
	} {
		ioctlSetWinsizeError := unix.IoctlSetWinsize(
			int(slave.Fd()),
			unix.TIOCSWINSZ,
			&winsize,
		)
		if ioctlSetWinsizeError != nil {
			t.Fatal(ioctlSetWinsizeError)
		}

		resized, err := readSize(int(slave.Fd()), size)
		if err != nil || resized.ready() || resized.cellWidth != 0 ||
			resized.cellHeight != 0 ||
			resized.columns != int(winsize.Col) ||
			resized.rows != int(winsize.Row) {
			t.Fatal(resized, err)
		}
	}
}

func TestPasswordClearFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		verifyPasswordClearFailure(t, passwordClearCase{canceled: canceled})
	}
}

func verifyTerminalTransitions(
	t *testing.T,
	session *terminalSession,
	output *bytes.Buffer,
) {
	t.Helper()

	err := session.enableInput()
	if err != nil {
		t.Fatal(err)
	}

	cleanupStart := output.Len()

	err = session.close()
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(
		output.String()[cleanupStart:],
		"\x1b\\\x1b[?2026l\x1b_Ga=d",
	) {
		t.Fatal(
			"cleanup did not end synchronization before deleting image",
		)
	}

	err = session.close()
	if err != nil {
		t.Fatal(err)
	}

	for _, sequence := range []string{
		enterScreen,
		enterInput,
		leaveInput,
		leaveScreen,
	} {
		if !strings.Contains(output.String(), sequence) {
			t.Fatal("missing terminal transition")
		}
	}
}

func verifyLiveSession(
	t *testing.T,
	mode string,
	master, slave *os.File,
	command *exec.Cmd,
	outputText func() string,
	recording *wireRecording,
) {
	t.Helper()

	status := "Ctrl-] quit | interactive | 0.0 FPS"
	if mode == "readonly" {
		status = "Ctrl-] quit | readonly | 0.0 FPS"
	}

	if !strings.Contains(
		outputText(),
		"\x1b[5;1H\x1b[2K"+status+"\x1b[H",
	) {
		t.Fatal("missing live quit hint and mode")
	}

	if mode == "quit" || mode == "readonly" {
		verifyIdleFooter(t, outputText)
	}

	verifyViewportResize(t, slave, command, outputText, status)

	initialFrames := strings.Count(outputText(), "a=T")

	queries := strings.Count(outputText(), querySize)

	ioctlSetWinsizeError := unix.IoctlSetWinsize(
		int(slave.Fd()),
		unix.TIOCSWINSZ,
		&unix.Winsize{Col: 32, Row: 4},
	)
	if ioctlSetWinsizeError != nil {
		t.Fatal(ioctlSetWinsizeError)
	}

	ioctlSetWinsizeError = command.Process.Signal(unix.SIGWINCH)
	if ioctlSetWinsizeError != nil {
		t.Fatal(ioctlSetWinsizeError)
	}

	waitFor(
		t,
		func() bool { return strings.Count(outputText(), querySize) > queries },
	)

	_, writeStringError := master.WriteString(
		"\x1b[<0;2;2M\x1b[<0;2;2mz",
	)
	if writeStringError != nil {
		t.Fatal(writeStringError)
	}

	if mode != "readonly" {
		waitFor(t, func() bool {
			for _, msg := range recording.snapshot() {
				if msg[0] == 4 && msg[1] == 0 &&
					binary.BigEndian.Uint32(msg[4:]) == 'z' {
					return true
				}
			}

			return false
		})
	}

	time.Sleep(100 * time.Millisecond)

	if strings.Count(outputText(), "a=T") != initialFrames {
		t.Fatal("rendered with stale cell metrics")
	}

	for _, msg := range recording.snapshot() {
		if msg[0] == 5 {
			t.Fatalf(
				"pointer mapped with stale cell metrics: %x",
				msg,
			)
		}
	}

	report := "\x1b[6;20;10t"
	if mode == "readonly" || mode == "disconnect" {
		report = "\x1b[4;80;320t"
	}

	_, writeStringError = master.WriteString(report)
	if writeStringError != nil {
		t.Fatal(writeStringError)
	}

	waitFor(
		t,
		func() bool {
			return strings.Count(outputText(), "a=T") > initialFrames
		},
	)

	_, writeStringError = master.WriteString(
		"\x1b[99;5u\x1b[<0;2;2M\x1b[<32;3;2M\x1b[<64;3;2M",
	)
	if writeStringError != nil {
		t.Fatal(writeStringError)
	}

	if mode != "readonly" {
		waitFor(t, func() bool {
			for _, msg := range recording.snapshot() {
				if msg[0] == 4 && msg[1] == 1 &&
					binary.BigEndian.Uint32(msg[4:]) == 'c' {
					return true
				}
			}

			return false
		})
	}
}

func verifyReleasedInput(t *testing.T, mode string, recording *wireRecording) {
	t.Helper()

	messages := recording.snapshot()
	if mode == "readonly" {
		for _, msg := range messages {
			if msg[0] != 3 {
				t.Fatalf("readonly emitted input %x", msg)
			}
		}
	}

	if mode == "readonly" || mode == "disconnect" ||
		strings.HasPrefix(mode, "password-") {
		return
	}

	{
		keyUp, buttonUp := false, false

		for _, msg := range messages {
			if msg[0] == 4 && msg[1] == 0 &&
				binary.BigEndian.Uint32(msg[4:]) == 'c' {
				keyUp = true
			}

			if msg[0] == 5 && msg[1] == 0 {
				buttonUp = true
			}
		}

		if !keyUp || !buttonUp {
			t.Fatalf("held input not released: %x", messages)
		}
	}
}

func recordTerminal(t *testing.T, master *os.File) func() string {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	t.Cleanup(cancel)

	var (
		outputMutex sync.Mutex
		output      bytes.Buffer
	)

	outputText := func() string {
		outputMutex.Lock()
		defer outputMutex.Unlock()

		return output.String()
	}
	readerDone := make(chan struct{})

	go func() {
		defer close(readerDone)

		var data [4096]byte

		replied := false

		for {
			count, err := pollInput(ctx, int(master.Fd()), data[:])
			if err != nil {
				return
			}

			outputMutex.Lock()

			_, _ = output.Write(data[:count])

			text := output.String()
			outputMutex.Unlock()

			if !replied && strings.Contains(text, queryGraphics) {
				_, replyError := master.WriteString(
					"\x1b_Gi=31;OK\x1b\\\x1b[6;16;8t\x1b[?15u",
				)
				if replyError != nil {
					t.Error(replyError)

					return
				}

				replied = true
			}
		}
	}()

	t.Cleanup(func() { cancel(); <-readerDone })

	return outputText
}

type passwordClearCase struct{ canceled bool }

func verifyPasswordClearFailure(t *testing.T, testcase passwordClearCase) {
	t.Helper()

	pair := openPTY(t)
	master, slave := pair.master, pair.slave

	session, openTerminalError := openTerminal(slave, io.Discard)
	if openTerminalError != nil {
		t.Fatal(openTerminalError)
	}

	defer func() { _ = session.close() }()

	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	if testcase.canceled {
		cancel()
	} else {
		_, err := master.WriteString("secret\r")
		if err != nil {
			t.Fatal(err)
		}
	}

	calls := 0
	writer := renderWriterFunc(func(data []byte) (int, error) {
		calls++
		if calls == 1 {
			return len(data), nil
		}

		if string(data) != "\x1b[2J\x1b[H" {
			t.Fatalf("unexpected password cleanup: %q", data)
		}

		return 0, io.ErrClosedPipe
	})
	secret, openTerminalError := readPassword(ctx, int(slave.Fd()), writer)

	if secret != nil || !errors.Is(openTerminalError, io.ErrClosedPipe) ||
		calls != 2 {
		t.Fatalf(
			"cleanup failure lost: secret length %d, calls %d, error %v",
			len(secret),
			calls,
			openTerminalError,
		)
	}

	if testcase.canceled && !errors.Is(openTerminalError, context.Canceled) {
		t.Fatal("cleanup lost cancellation", openTerminalError)
	}
}

func verifyIdleFooter(t *testing.T, outputText func() string) {
	t.Helper()

	footerStart := beginSync + "\x1b[5;1H\x1b[2K"

	waitFor(
		t,
		func() bool { return strings.Contains(outputText(), footerStart) },
	)

	text := outputText()
	start := strings.Index(text, footerStart)

	if strings.Contains(text[start:], " | 0.0 FPS") ||
		!strings.Contains(text[start:], " FPS") {
		t.Fatal("displayed frames not measured", text[start:])
	}

	waitFor(
		t,
		func() bool {
			return strings.Contains(
				outputText()[start:],
				" | 0.0 FPS",
			)
		},
	)

	if strings.Contains(outputText()[start:], "\x1b_G") {
		t.Fatal("idle FPS update replaced or deleted image")
	}
}

func verifyViewportResize(
	t *testing.T,
	slave *os.File,
	command *exec.Cmd,
	outputText func() string,
	status string,
) {
	t.Helper()

	ioctlSetWinsizeError := unix.IoctlSetWinsize(
		int(slave.Fd()),
		unix.TIOCSWINSZ,
		&unix.Winsize{Col: 32, Row: 1, Xpixel: 256, Ypixel: 16},
	)
	if ioctlSetWinsizeError != nil {
		t.Fatal(ioctlSetWinsizeError)
	}

	ioctlSetWinsizeError = command.Process.Signal(unix.SIGWINCH)
	if ioctlSetWinsizeError != nil {
		t.Fatal(ioctlSetWinsizeError)
	}

	waitFor(t, func() bool {
		return strings.Contains(
			outputText(),
			"\x1b[1;1H\x1b[2K"+status[:min(len(status), 32)]+"\x1b[H",
		)
	})

	initialFrames := strings.Count(outputText(), "a=T")

	ioctlSetWinsizeError = unix.IoctlSetWinsize(
		int(slave.Fd()),
		unix.TIOCSWINSZ,
		&unix.Winsize{Col: 32, Row: 4, Xpixel: 256, Ypixel: 64},
	)
	if ioctlSetWinsizeError != nil {
		t.Fatal(ioctlSetWinsizeError)
	}

	ioctlSetWinsizeError = command.Process.Signal(unix.SIGWINCH)
	if ioctlSetWinsizeError != nil {
		t.Fatal(ioctlSetWinsizeError)
	}

	waitFor(
		t,
		func() bool {
			return strings.Count(outputText(), "a=T") > initialFrames
		},
	)

	if !strings.Contains(
		outputText(),
		"\x1b[4;1H\x1b[2K"+status[:min(len(status), 32)]+"\x1b[H",
	) {
		t.Fatal("footer not redrawn after resize")
	}
}
