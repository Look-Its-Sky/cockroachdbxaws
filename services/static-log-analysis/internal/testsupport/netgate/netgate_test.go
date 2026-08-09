package netgate_test

import (
	"bufio"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/netgate"
)

// echoServer is a stand-in upstream. The gate is what is under test, so the
// thing behind it only has to be recognisable.
func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener.Addr().String()
}

func TestAnOpenGateForwardsBothDirections(t *testing.T) {
	gate, err := netgate.New(echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gate.Close)

	conn, err := net.DialTimeout("tcp", gate.Addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "hello\n" {
		t.Fatalf("read %q", line)
	}
}

// TestBlockingSeversEstablishedConnections is what makes this usable as an
// outage: a pool that already holds connections must lose them, not keep using
// them.
func TestBlockingSeversEstablishedConnections(t *testing.T) {
	gate, err := netgate.New(echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gate.Close)

	conn, err := net.DialTimeout("tcp", gate.Addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	gate.Block()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 16)); err == nil {
		t.Fatal("an established connection survived the gate closing")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("an established connection was left hanging rather than severed")
	}
}

func TestABlockedGateRefusesNewConnectionsAndReopens(t *testing.T) {
	gate, err := netgate.New(echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gate.Close)

	gate.Block()
	if !gate.Blocked() {
		t.Fatal("Blocked() disagrees with Block()")
	}
	conn, err := net.DialTimeout("tcp", gate.Addr(), 5*time.Second)
	if err == nil {
		// A listener with nothing behind it accepts and closes, so the refusal
		// is observed on the first read rather than on dial.
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := conn.Read(make([]byte, 16)); err == nil {
			t.Fatal("a blocked gate served a new connection")
		} else if !errors.Is(err, io.EOF) {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatal("a blocked gate left a new connection hanging")
			}
		}
		_ = conn.Close()
	}

	gate.Unblock()
	reopened, err := net.DialTimeout("tcp", gate.Addr(), 5*time.Second)
	if err != nil {
		t.Fatalf("the gate did not reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.Write([]byte("again\n")); err != nil {
		t.Fatal(err)
	}
	_ = reopened.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(reopened).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "again\n" {
		t.Fatalf("read %q after reopening", line)
	}
}
