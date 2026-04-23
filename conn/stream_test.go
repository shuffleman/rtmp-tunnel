package conn

import (
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/shuffleman/rtmp-tunnel/config"
)

func TestStreamIntegrity(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.AppName = "live"
	cfg.Encryption.Enable = true
	cfg.Encryption.Algorithm = "aes-256-gcm"
	cfg.Encryption.Key = []byte("test-secret-key")
	cfg.FrameRate = 240
	cfg.TargetBitrate = 500000000
	cfg.FlushInterval = 1 * time.Millisecond
	cfg.WriteBatchSize = 256 * 1024
	cfg.TimestampJitterMax = 0

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	serverRead := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer c.Close()
		srv := NewServerConn(cfg)
		if err := srv.Wrap(c, true); err != nil {
			serverErr <- err
			return
		}
		want := 8 * 1024
		buf := make([]byte, want)
		_, err = io.ReadFull(srv, buf)
		if err != nil {
			serverErr <- err
			return
		}
		serverRead <- buf
		serverErr <- nil
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	cli := NewClientConn(cfg)
	if err := cli.Wrap(raw, false); err != nil {
		t.Fatal(err)
	}

	payload := make([]byte, 8*1024)
	_, _ = rand.Read(payload)
	_, err = cli.Write(payload)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout")
	}

	got := <-serverRead
	if len(got) != len(payload) {
		t.Fatalf("length mismatch: got %d want %d", len(got), len(payload))
	}
	for i := range payload {
		if got[i] != payload[i] {
			t.Fatalf("byte mismatch at %d: got %d want %d", i, got[i], payload[i])
		}
	}
}
