// tnc-server bridges a KISS TNC to multiple TCP clients,
// allowing several applications to share a single packet radio TNC.
// The TNC can be connected via serial port or TCP (e.g. Direwolf, soundmodem).
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

const (
	minKISSFrameSize = 15   // smallest KISS packet we expect to see
	kissDelimiter    = 0xc0 // KISS frame boundary marker
	reconnectDelay   = 5 * time.Second
)

// connector returns a new connection to the TNC device.
type connector func(ctx context.Context) (io.ReadWriteCloser, error)

func serialConnector(portName string, baud int) connector {
	return func(_ context.Context) (io.ReadWriteCloser, error) {
		return serial.Open(portName, &serial.Mode{BaudRate: baud})
	}
}

func tcpConnector(addr string) connector {
	return func(ctx context.Context) (io.ReadWriteCloser, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

// broadcaster distributes frames from one producer to many consumers.
type broadcaster struct {
	mu        sync.RWMutex
	consumers map[chan []byte]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{
		consumers: make(map[chan []byte]struct{}),
	}
}

func (b *broadcaster) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	b.mu.Lock()
	b.consumers[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broadcaster) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.consumers, ch)
	b.mu.Unlock()
	close(ch)
}

func (b *broadcaster) send(data []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.consumers {
		select {
		case ch <- data:
		default:
			// drop frame if consumer can't keep up
		}
	}
}

func main() {
	portName := flag.String("port", "/dev/ttyUSB0", "serial port or tcp:host:port for network KISS")
	baud := flag.Int("baud", 4800, "baud rate for serial device (ignored for tcp)")
	listenAddr := flag.String("listen", ":6700", "address:port to listen on")
	debug := flag.Bool("debug", false, "enable debug output")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var (
		connect   connector
		reconnect bool
	)
	if addr, ok := strings.CutPrefix(*portName, "tcp:"); ok {
		connect = tcpConnector(addr)
		reconnect = true
		slog.Info("using TCP KISS target", "addr", addr)
	} else {
		connect = serialConnector(*portName, *baud)
		slog.Info("using serial port", "port", *portName, "baud", *baud)
	}

	if err := serve(ctx, *listenAddr, connect, reconnect, *debug); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, addr string, connect connector, reconnect, debug bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()

	slog.Info("listening for connections", "addr", addr)

	tncCtx, tncCancel := context.WithCancel(ctx)
	defer tncCancel()

	bc := newBroadcaster()
	writeCh := make(chan []byte, 15)

	go func() {
		manageTNC(tncCtx, connect, reconnect, bc, writeCh)
		tncCancel()
	}()

	go func() {
		<-tncCtx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if tncCtx.Err() != nil {
				slog.Info("shutting down")
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		slog.Info("client connected", "remote", conn.RemoteAddr())

		consumer := bc.subscribe()
		go forwardToClient(conn, consumer, bc)
		go forwardFromClient(conn, writeCh, debug)
	}
}

// manageTNC maintains the connection to the TNC device. For TCP targets,
// it automatically reconnects on failure. For serial, it exits on error.
func manageTNC(ctx context.Context, connect connector, reconnect bool, bc *broadcaster, writeCh <-chan []byte) {
	for {
		tnc, err := connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !reconnect {
				slog.Error("TNC connection failed", "error", err)
				return
			}
			slog.Error("TNC connection failed, retrying", "error", err, "delay", reconnectDelay)
			select {
			case <-time.After(reconnectDelay):
				continue
			case <-ctx.Done():
				return
			}
		}

		slog.Info("TNC connected")

		readDone := make(chan struct{})
		writeDone := make(chan struct{})
		go func() {
			readTNC(tnc, bc)
			close(readDone)
		}()
		go func() {
			writeTNC(tnc, writeCh, readDone)
			close(writeDone)
		}()

		<-readDone
		tnc.Close()
		<-writeDone

		if !reconnect {
			slog.Error("TNC connection lost")
			return
		}
		slog.Warn("TNC connection lost, reconnecting", "delay", reconnectDelay)
		select {
		case <-time.After(reconnectDelay):
		case <-ctx.Done():
			return
		}
	}
}

// readTNC reads KISS frames from the TNC and broadcasts them to all clients.
func readTNC(tnc io.Reader, bc *broadcaster) {
	r := bufio.NewReader(tnc)
	for {
		var frame []byte
		for len(frame) <= minKISSFrameSize {
			var err error
			frame, err = r.ReadBytes(kissDelimiter)
			if err != nil {
				slog.Error("TNC read error", "error", err)
				return
			}
		}
		out := make([]byte, len(frame))
		copy(out, frame)
		bc.send(out)
	}
}

// forwardToClient sends broadcast frames to a single TCP client.
func forwardToClient(conn net.Conn, consumer chan []byte, bc *broadcaster) {
	defer conn.Close()
	defer bc.unsubscribe(consumer)

	for frame := range consumer {
		if _, err := conn.Write(frame); err != nil {
			slog.Info("client disconnected", "remote", conn.RemoteAddr(), "error", err)
			return
		}
	}
}

// forwardFromClient reads KISS frames from a TCP client and queues them for TNC output.
func forwardFromClient(conn net.Conn, writeCh chan<- []byte, debug bool) {
	r := bufio.NewReader(conn)
	for {
		firstByte, err := r.ReadByte()
		if err != nil {
			slog.Info("client disconnected", "remote", conn.RemoteAddr(), "error", err)
			return
		}

		var buf bytes.Buffer
		buf.WriteByte(firstByte)

		var frame []byte
		for len(frame) <= minKISSFrameSize {
			frame, err = r.ReadBytes(kissDelimiter)
			if err != nil {
				slog.Info("client disconnected", "remote", conn.RemoteAddr(), "error", err)
				return
			}

			if debug {
				dumpFrame(frame)
			}
		}

		buf.Write(frame)
		writeCh <- buf.Bytes()
	}
}

// writeTNC drains the write channel and sends frames to the TNC.
// It stops when the stop channel is closed (indicating the read side died).
func writeTNC(tnc io.Writer, writeCh <-chan []byte, stop <-chan struct{}) {
	for {
		select {
		case frame := <-writeCh:
			if _, err := tnc.Write(frame); err != nil {
				slog.Error("TNC write error", "error", err)
				return
			}
		case <-stop:
			return
		}
	}
}

func dumpFrame(frame []byte) {
	fmt.Println("Byte#\tHexVal\tChar\tChar>>1\tBinary")
	fmt.Println("-----\t------\t----\t-------\t------")
	for i, b := range frame {
		fmt.Printf("%4d \t%#x \t%v \t%v\t%08b\n", i, b, string(b), string(b>>1), b)
	}
}
