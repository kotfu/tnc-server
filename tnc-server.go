// tnc-server bridges a KISS TNC serial device to multiple TCP clients,
// allowing several applications to share a single packet radio TNC.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"

	"go.bug.st/serial"
)

const (
	minKISSFrameSize = 15   // smallest KISS packet we expect to see
	kissDelimiter    = 0xc0 // KISS frame boundary marker
)

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
	portName := flag.String("port", "/dev/ttyUSB0", "serial port device")
	baud := flag.Int("baud", 4800, "baud rate for serial device")
	listenAddr := flag.String("listen", ":6700", "address:port to listen on")
	debug := flag.Bool("debug", false, "enable debug output")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	port, err := serial.Open(*portName, &serial.Mode{BaudRate: *baud})
	if err != nil {
		slog.Error("failed to open serial port", "port", *portName, "error", err)
		os.Exit(1)
	}
	defer port.Close()

	if err := serve(ctx, *listenAddr, port, *debug); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, addr string, port serial.Port, debug bool) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	defer ln.Close()

	slog.Info("listening for connections", "addr", addr)

	bc := newBroadcaster()
	writeCh := make(chan []byte, 15)

	go readSerial(port, bc)
	go writeSerial(port, writeCh)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
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

// readSerial reads KISS frames from the serial port and broadcasts them to all clients.
func readSerial(port serial.Port, bc *broadcaster) {
	r := bufio.NewReader(port)
	for {
		var frame []byte
		for len(frame) <= minKISSFrameSize {
			var err error
			frame, err = r.ReadBytes(kissDelimiter)
			if err != nil {
				slog.Error("serial read error", "error", err)
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

// forwardFromClient reads KISS frames from a TCP client and queues them for serial output.
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

// writeSerial drains the write channel and sends frames to the serial port.
func writeSerial(port serial.Port, writeCh <-chan []byte) {
	for frame := range writeCh {
		if _, err := port.Write(frame); err != nil {
			slog.Error("serial write error", "error", err)
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
