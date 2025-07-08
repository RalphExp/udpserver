package udp

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/panjf2000/ants/v2"
	"github.com/stretchr/testify/assert"
)

type UDPHandler struct {
}

func (h *UDPHandler) OnDatagram(s Server,
	c Connection, buf []byte, sz int) {
	c.WriteTo(append([]byte("echo:"), buf[:sz]...), c.RemoteAddr())
}

func (h *UDPHandler) OnTimer(s Server) {
	fmt.Printf("On Timer: %v\n", time.Now())
}

type UDPHandler2 struct {
}

// Test Connection
func (*UDPHandler2) OnDatagram(s Server, conn Connection, buf []byte, sz int) {
	conn.WriteTo(buf[:sz], conn.RemoteAddr())
}

func (*UDPHandler2) OnTimer(s Server) {}

// Send echo to server and receive the same echo string
//
// Expect: no error send/recv/Close
func TestUDPServer_BasicEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := NewUDPServer(ctx,
		WithWorkerPoolSize(1024*1024),
		WithTickInterval(100*time.Millisecond),
		WithLogger(log.Default()),
	)

	go func() {
		if err := server.Serve("127.0.0.1:9090", &UDPHandler{}); err != nil {
			t.Errorf("Failed to start server: %v", err)
		}
	}()

	// wait for server to start
	time.Sleep(100 * time.Millisecond)

	// test client
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 9090,
	})

	if err != nil {
		t.Fatalf("Failed to dial UDP: %v", err)
	}
	defer conn.Close()

	message := []byte("hello")
	_, err = conn.Write(message)
	if err != nil {
		t.Fatalf("Failed to write to UDP: %v", err)
	}

	buf := make([]byte, 1024)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("Failed to read from UDP: %v", err)
	}

	expected := "echo:hello"
	if string(buf[:n]) != expected {
		t.Errorf("Expected '%s', got '%s'", expected, string(buf[:n]))
	}

	aerr := server.Stop()
	if aerr != nil {
		t.Errorf("Failed to stop server: %v", aerr.Error())
	}
}

// Send echo to server and receive the same echo string
//
// Expect: no error send/recv/Close
func TestUDPServer_BasicEcho2(t *testing.T) {
	server := NewUDPServer(context.Background(),
		WithWorkerPoolSize(1024*1024),
		WithTickInterval(100*time.Millisecond),
		WithLogger(log.Default()),
	)

	go func() {
		if err := server.Serve("127.0.0.1:9090", &UDPHandler2{}); err != nil {
			t.Errorf("Failed to start server: %v", err)
		}
	}()

	// wait for server to start
	time.Sleep(100 * time.Millisecond)
	// test client
	conn, _ := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: 9090,
	})
	defer conn.Close()

	message := []byte("hello")
	_, err := conn.Write(message)
	if err != nil {
		t.Fatalf("Failed to write to UDP: %v", err)
	}

	buf := make([]byte, 1024)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("Failed to read from UDP: %v", err)
	}

	expected := "hello"
	if string(buf[:n]) != expected {
		t.Errorf("Expected '%s', got '%s'", expected, string(buf[:n]))
	}
	server.Stop()
}

// Test that server can not start if address is not correct
//
// Expect: Serve returns error
func TestUDPServer_PortError(t *testing.T) {
	addr := "127.0.0.1:70000"
	ctx := context.Background()
	server := NewUDPServer(ctx)
	aerr := server.Serve(addr, &UDPHandler{})
	if aerr == nil {
		t.Fatalf("Can not bind error port")
	}
}

// Test that server can not reuse the port
//
// Expect: Serve returns error
func TestUDPServer_PortAlreadyInUse(t *testing.T) {
	addr := "127.0.0.1:9094"
	l, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Fatalf("Failed to bind test port: %v", err)
	}
	defer l.Close()

	ctx := context.Background()
	server := NewUDPServer(ctx)

	aerr := server.Serve(addr, &UDPHandler{})
	if aerr == nil {
		t.Error("Expected error when port is already in use, got nil")
	}
}

// Test if Server is stoppable from outside
//
// Expect: no timeout occurs
func TestUDPServer_Stoppable(t *testing.T) {
	addr := "127.0.0.1:9094"
	ctx := context.Background()
	server := NewUDPServer(ctx, WithTickInterval(1*time.Second))

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		server.Serve(addr, &UDPHandler{})
	}()

	// let the server start
	time.Sleep(500 * time.Millisecond)
	server.Stop()
	wg.Wait()
}

// Test if Server.Stop() is imdepotent
//
// Expect: Stop() can be called multiple times without crashing
func TestUDPServer_MultipleStop(t *testing.T) {
	addr := "127.0.0.1:9094"
	ctx := context.Background()
	server := NewUDPServer(ctx, WithTickInterval(1*time.Second))

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		server.Serve(addr, &UDPHandler{})
	}()

	// let the server start
	time.Sleep(100 * time.Millisecond)

	// multiple Stop must be safe
	arr := server.Stop()
	assert.Nil(t, arr, "Stop should not return error")

	arr = server.Stop()
	assert.NotNil(t, arr, "Stop should return error")
	wg.Wait()
}

// Test Server can not close the pool
//
// Expect: workerPool.IsClosed() == false
func TestUDPServer_AttachPool(t *testing.T) {
	addr := "127.0.0.1:9094"
	ctx := context.Background()

	pool, _ := ants.NewPool(1024)
	server := NewUDPServer(ctx,
		WithTickInterval(1*time.Second),
		WithWorkerPool(pool))

	go func() {
		server.Serve(addr, &UDPHandler{})
	}()

	// let the server start
	time.Sleep(100 * time.Millisecond)
	server.Stop()

	assert.True(t, server.(*UDPServer).workerPool.IsClosed() == false)
}
