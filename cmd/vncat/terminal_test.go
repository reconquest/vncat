package main

import (
	"bytes"
	"context"
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

	uv "github.com/charmbracelet/ultraviolet"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 20, Row: 5, Xpixel: 160, Ypixel: 80}); err != nil {
		t.Fatal(err)
	}
	return master, slave
}

func TestTerminalRestorationAndOutputFailure(t *testing.T) {
	_, slave := openPTY(t)
	before, err := term.GetState(int(slave.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		var output bytes.Buffer
		writer := io.Writer(&output)
		if fail {
			writer = failedWriter{}
		}
		session, err := openTerminal(slave, writer)
		if fail {
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			if err := session.enableInput(); err != nil {
				t.Fatal(err)
			}
			if err := session.close(); err != nil {
				t.Fatal(err)
			}
			if err := session.close(); err != nil {
				t.Fatal(err)
			}
			for _, sequence := range []string{enterScreen, enterInput, leaveInput, leaveScreen} {
				if !strings.Contains(output.String(), sequence) {
					t.Fatal("missing terminal transition")
				}
			}
		}
		after, err := term.GetState(int(slave.Fd()))
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("terminal not restored", err)
		}
	}
}

func TestPasswordAndInputCancellation(t *testing.T) {
	master, slave := openPTY(t)
	var output bytes.Buffer
	session, err := openTerminal(slave, &output)
	if err != nil {
		t.Fatal(err)
	}
	defer session.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	passwordDone := make(chan []byte, 1)
	go func() {
		secret, err := readPassword(ctx, int(slave.Fd()), &output)
		if err != nil {
			t.Error(err)
		}
		passwordDone <- secret
	}()
	if _, err := master.Write([]byte("sëx\x7fcret\r")); err != nil {
		t.Fatal(err)
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
		if _, err := master.Write(piece); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
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
			os.Exit(0)
		}
	}
	os.Exit(2)
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

func mockServer(conn net.Conn, recording *wireRecording) error {
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, "RFB 003.008\n"); err != nil {
		return err
	}
	var version [12]byte
	if _, err := io.ReadFull(conn, version[:]); err != nil {
		return err
	}
	if string(version[:]) != "RFB 003.008\n" {
		return errors.New("bad client version")
	}
	if _, err := conn.Write([]byte{1, 1}); err != nil {
		return err
	}
	var selected [1]byte
	if _, err := io.ReadFull(conn, selected[:]); err != nil {
		return err
	}
	if selected[0] != 1 {
		return errors.New("bad security selection")
	}
	if _, err := conn.Write([]byte{0, 0, 0, 0}); err != nil {
		return err
	}
	if _, err := io.ReadFull(conn, selected[:]); err != nil {
		return err
	}
	if _, err := conn.Write(serverInit(2, 2)); err != nil {
		return err
	}
	setup := make([]byte, 44)
	if _, err := io.ReadFull(conn, setup); err != nil {
		return err
	}
	if setup[0] != 0 || setup[20] != 2 {
		return errors.New("bad initialization")
	}
	updates := 0
	for {
		var kind [1]byte
		if _, err := io.ReadFull(conn, kind[:]); err != nil {
			return err
		}
		sizes := map[byte]int{3: 9, 4: 7, 5: 5}
		length, ok := sizes[kind[0]]
		if !ok {
			return fmt.Errorf("unexpected message %d", kind[0])
		}
		data := make([]byte, length+1)
		data[0] = kind[0]
		if _, err := io.ReadFull(conn, data[1:]); err != nil {
			return err
		}
		recording.add(data)
		if kind[0] == 3 && updates < 2 {
			var update []byte
			if updates == 0 {
				update = updateData(rectangle(0, 0, 2, 2, 0, bytes.Repeat([]byte{0, 0, 255, 0}, 4)))
			} else {
				update = updateData(rectangle(0, 0, 4, 2, -223, nil), rectangle(0, 0, 4, 2, 0, bytes.Repeat([]byte{0, 255, 0, 0}, 8)))
			}
			if _, err := conn.Write(append([]byte{0}, update...)); err != nil {
				return err
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
	for _, mode := range []string{"quit", "readonly", "signal", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			master, slave := openPTY(t)
			original, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
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
				serverDone <- mockServer(conn, recording)
			}()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"-test.run=^TestCommandProcess$", "--", listener.Addr().String()}
			if mode == "readonly" {
				args = append(args, "-R")
			}
			command := exec.Command(executable, args...)
			command.Env = append(os.Environ(), "VNCTTY_TEST_PROCESS=1")
			command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			defer command.Process.Kill()
			processDone := make(chan error, 1)
			go func() { processDone <- command.Wait() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var outputMutex sync.Mutex
			var output bytes.Buffer
			outputText := func() string { outputMutex.Lock(); defer outputMutex.Unlock(); return output.String() }
			readerDone := make(chan struct{})
			go func() {
				defer close(readerDone)
				var data [4096]byte
				replied := false
				for {
					n, err := pollInput(ctx, int(master.Fd()), data[:])
					if err != nil {
						return
					}
					outputMutex.Lock()
					output.Write(data[:n])
					text := output.String()
					outputMutex.Unlock()
					if !replied && strings.Contains(text, queryGraphics) {
						master.Write([]byte("\x1b_Gi=31;OK\x1b\\\x1b[6;16;8t\x1b[?15u"))
						replied = true
					}
				}
			}()
			defer func() { cancel(); <-readerDone }()
			var conn net.Conn
			select {
			case conn = <-connected:
			case <-time.After(3 * time.Second):
				t.Fatal("no VNC connection")
			}
			defer conn.Close()
			waitFor(t, func() bool { return strings.Contains(outputText(), "a=T") })
			initialFrames := strings.Count(outputText(), "a=T")
			if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 12, Row: 4, Xpixel: 96, Ypixel: 64}); err != nil {
				t.Fatal(err)
			}
			if err := command.Process.Signal(unix.SIGWINCH); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return strings.Count(outputText(), "a=T") > initialFrames })
			if _, err := master.Write([]byte("\x1b[99;5u\x1b[<0;2;2M\x1b[<32;3;2M\x1b[<64;3;2M")); err != nil {
				t.Fatal(err)
			}
			if mode != "readonly" {
				waitFor(t, func() bool {
					for _, msg := range recording.snapshot() {
						if msg[0] == 4 && msg[1] == 1 && wireOrder.Uint32(msg[4:]) == 'c' {
							return true
						}
					}
					return false
				})
			}
			switch mode {
			case "signal":
				err = command.Process.Signal(unix.SIGTERM)
			case "disconnect":
				err = conn.Close()
			default:
				_, err = master.Write([]byte("\x1b[93;5u"))
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-processDone:
				if mode != "disconnect" && err != nil {
					t.Fatalf("process failed: %v %q", err, outputText())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("client did not exit")
			}
			waitFor(t, func() bool { return strings.Contains(outputText(), leaveScreen) })
			after, err := term.GetState(int(slave.Fd()))
			if err != nil || !reflect.DeepEqual(original, after) {
				t.Fatal("process left terminal raw", err)
			}
			select {
			case <-serverDone:
			case <-time.After(time.Second):
				t.Fatal("server connection leaked")
			}
			if !strings.Contains(outputText(), leaveInput) {
				t.Fatal("input modes not restored")
			}
			messages := recording.snapshot()
			if mode == "readonly" {
				for _, msg := range messages {
					if msg[0] != 3 {
						t.Fatalf("readonly emitted input %x", msg)
					}
				}
			} else if mode != "disconnect" {
				keyUp, buttonUp := false, false
				for _, msg := range messages {
					if msg[0] == 4 && msg[1] == 0 && wireOrder.Uint32(msg[4:]) == 'c' {
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
		})
	}
}

func TestServeFailureCleanup(t *testing.T) {
	for _, mode := range []string{"graphics-error", "cancel", "missing-metrics"} {
		t.Run(mode, func(t *testing.T) {
			master, slave := openPTY(t)
			if mode == "missing-metrics" {
				err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ,
					&unix.Winsize{Col: 20, Row: 5})
				if err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			terminal, err := openTerminal(slave, &output)
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.close()
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			client := &rfbClient{conn: clientConn}
			if err := client.resize(2, 2); err != nil {
				t.Fatal(err)
			}
			peerDone := make(chan struct{})
			go func() { io.Copy(io.Discard, serverConn); close(peerDone) }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- serve(ctx, client, terminal, nil) }()
			switch mode {
			case "graphics-error":
				_, err = master.Write([]byte("\x1b_Gi=31;ENOTSUP\x1b\\"))
			case "cancel":
				cancel()
			case "missing-metrics":
				_, err = master.Write([]byte("\x1b_Gi=31;OK\x1b\\"))
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				switch mode {
				case "cancel":
					if err != nil {
						t.Fatal(err)
					}
				case "graphics-error":
					if err == nil || !strings.Contains(err.Error(), "rejected Kitty") {
						t.Fatal(err)
					}
				case "missing-metrics":
					if err == nil || !strings.Contains(err.Error(), "pixel dimensions") {
						t.Fatal(err)
					}
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
