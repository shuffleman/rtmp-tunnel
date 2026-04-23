package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/conn"
)

func main() {
	var (
		mode          = flag.String("mode", "tcp", "tcp|rtmp|socks")
		server        = flag.String("s", "127.0.0.1:19000", "server address (tcp/rtmp)")
		proxy         = flag.String("proxy", "127.0.0.1:1080", "socks5 proxy address (socks)")
		target        = flag.String("target", "127.0.0.1:19000", "target address (socks)")
		seconds       = flag.Int("t", 10, "duration seconds")
		drainSec      = flag.Int("drain", 1, "drain seconds after stop writing")
		syncIO        = flag.Bool("sync", true, "write then read exact echo per chunk")
		window        = flag.Int("window", 1, "max in-flight chunks (sync)")
		handshakeOnly = flag.Bool("handshake", false, "handshake only (rtmp/socks/tcp)")
		chunk         = flag.Int("chunk", 256*1024, "chunk size")
		appName       = flag.String("app", "live", "app name (rtmp)")
		key           = flag.String("key", "", "encryption key (rtmp), supports base64:/hex: prefix")
		bitrate       = flag.Int("bitrate", 5000000, "target bitrate (rtmp)")
		fps           = flag.Int("fps", 30, "frame rate (rtmp)")
		maxSEI        = flag.Int("maxsei", 1400, "max sei size (rtmp)")
		seiPerF       = flag.Int("seiperframe", 4, "max sei per frame (rtmp)")
		flushMS       = flag.Int("flushms", 5, "flush interval ms (rtmp)")
		batch         = flag.Int("batch", 65536, "write batch size (rtmp)")
		parallel      = flag.Int("p", 1, "parallel connections")
		cpuProfile    = flag.String("cpuprofile", "", "write CPU profile to file")
	)
	flag.Parse()

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			log.Fatalf("create cpuprofile: %v", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatalf("start cpuprofile: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*seconds)*time.Second)
	defer cancel()

	var totalWrite atomic.Uint64
	var totalRead atomic.Uint64
	var totalOK atomic.Uint64
	var totalFail atomic.Uint64

	errCh := make(chan error, *parallel)
	for i := 0; i < *parallel; i++ {
		go func() {
			c, err := dial(*mode, *server, *proxy, *target, *appName, *key, *bitrate, *fps, *maxSEI, *seiPerF, *flushMS, *batch)
			if err != nil {
				totalFail.Add(1)
				errCh <- err
				return
			}
			defer c.Close()
			totalOK.Add(1)
			if *handshakeOnly {
				<-ctx.Done()
				errCh <- nil
				return
			}
			if *syncIO {
				errCh <- runSyncWindow(ctx, c, *chunk, *window, &totalWrite, &totalRead)
			} else {
				errCh <- run(ctx, c, *chunk, time.Duration(*drainSec)*time.Second, &totalWrite, &totalRead)
			}
		}()
	}

	start := time.Now()
	var lastErr error
	for i := 0; i < *parallel; i++ {
		if err := <-errCh; err != nil {
			lastErr = err
		}
	}
	elapsed := time.Since(start)
	w := totalWrite.Load()
	r := totalRead.Load()
	okCount := totalOK.Load()
	failCount := totalFail.Load()
	if lastErr != nil {
		log.Printf("bench error: %v", lastErr)
	}
	fmt.Printf("mode=%s parallel=%d ok=%d fail=%d duration=%s queued=%dB acked=%dB queued_up=%.2fMbps goodput=%.2fMbps\n",
		*mode,
		*parallel,
		okCount,
		failCount,
		elapsed.Truncate(time.Millisecond),
		w,
		r,
		float64(w)*8/elapsed.Seconds()/1e6,
		float64(r)*8/elapsed.Seconds()/1e6,
	)
}

func run(ctx context.Context, c net.Conn, chunk int, drain time.Duration, totalWrite, totalRead *atomic.Uint64) error {
	payload := make([]byte, chunk)
	_, _ = rand.Read(payload)

	readBuf := make([]byte, 256*1024)
	doneRead := make(chan struct{})
	readErrCh := make(chan error, 1)

	go func() {
		defer close(doneRead)
		for {
			n, err := c.Read(readBuf)
			if n > 0 {
				totalRead.Add(uint64(n))
			}
			if err != nil {
				select {
				case readErrCh <- err:
				default:
				}
				return
			}
		}
	}()

	for {
		select {
		case err := <-readErrCh:
			return err
		case <-ctx.Done():
			if drain > 0 {
				timer := time.NewTimer(drain)
				select {
				case <-doneRead:
				case <-timer.C:
				}
				timer.Stop()
			}
			return nil
		default:
		}
		n, err := c.Write(payload)
		if n > 0 {
			totalWrite.Add(uint64(n))
		}
		if err != nil {
			return err
		}
	}
}

func runSyncWindow(ctx context.Context, c net.Conn, chunk int, window int, totalWrite, totalRead *atomic.Uint64) error {
	if window <= 0 {
		window = 1
	}
	payload := make([]byte, chunk)
	_, _ = rand.Read(payload)

	readBuf := make([]byte, 256*1024)
	var inFlight uint64
	for {
		for inFlight < uint64(window) {
			select {
			case <-ctx.Done():
				goto drain
			default:
			}
			_ = c.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
			n, err := c.Write(payload)
			_ = c.SetWriteDeadline(time.Time{})
			if n > 0 {
				totalWrite.Add(uint64(n))
				inFlight++
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return err
			}
		}

		for inFlight > 0 {
			select {
			case <-ctx.Done():
				goto drain
			default:
			}
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			rn, rerr := c.Read(readBuf)
			_ = c.SetReadDeadline(time.Time{})
			if rn > 0 {
				totalRead.Add(uint64(rn))
			}
			if rerr != nil {
				if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
					break
				}
				return rerr
			}
			if rn > 0 {
				need := uint64(rn)
				for need > 0 && inFlight > 0 {
					if need >= uint64(chunk) {
						need -= uint64(chunk)
						inFlight--
					} else {
						need = 0
					}
				}
			}
		}
	}

drain:
	for {
		if inFlight == 0 {
			return nil
		}
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		rn, rerr := c.Read(readBuf)
		_ = c.SetReadDeadline(time.Time{})
		if rn > 0 {
			totalRead.Add(uint64(rn))
			need := uint64(rn)
			for need > 0 && inFlight > 0 {
				if need >= uint64(chunk) {
					need -= uint64(chunk)
					inFlight--
				} else {
					need = 0
				}
			}
		}
		if rerr != nil {
			if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
				return nil
			}
			return rerr
		}
	}
}

func dial(mode, server, proxyAddr, targetAddr, appName, key string, bitrate, fps, maxSEI, seiPerFrame, flushMS, batch int) (net.Conn, error) {
	switch strings.ToLower(mode) {
	case "tcp":
		return net.Dial("tcp", server)
	case "socks":
		return dialSOCKS5(proxyAddr, targetAddr)
	case "rtmp":
		raw, err := net.Dial("tcp", server)
		if err != nil {
			return nil, err
		}
		cfg := config.DefaultConfig()
		cfg.AppName = appName
		cfg.TargetBitrate = bitrate
		cfg.FrameRate = fps
		cfg.MaxSEISize = maxSEI
		cfg.MaxSEIPerFrame = seiPerFrame
		cfg.FlushInterval = time.Duration(flushMS) * time.Millisecond
		cfg.WriteBatchSize = batch
		cfg.Encryption.Enable = true
		cfg.Encryption.Algorithm = "aes-256-gcm"
		if key != "" {
			k, err := parseKey(key)
			if err != nil {
				raw.Close()
				return nil, err
			}
			cfg.Encryption.Key = k
		} else {
			cfg.Encryption.Key = []byte("test-secret-key")
		}
		cfg.IsServer = false
		rtmpConn := conn.NewClientConn(cfg)
		if err := rtmpConn.Wrap(raw, false); err != nil {
			raw.Close()
			return nil, err
		}
		return rtmpConn, nil
	default:
		return nil, fmt.Errorf("unknown mode: %s", mode)
	}
}

func dialSOCKS5(proxyAddr, targetAddr string) (net.Conn, error) {
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return nil, err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil {
		return nil, err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return nil, fmt.Errorf("socks auth method not accepted")
	}

	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return nil, err
	}
	port64, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return nil, err
	}
	port := uint16(port64)

	var req []byte
	ip := net.ParseIP(host)
	if ip4 := ip.To4(); ip4 != nil {
		req = append(req, 0x05, 0x01, 0x00, 0x01)
		req = append(req, ip4...)
	} else if ip6 := ip.To16(); ip6 != nil {
		req = append(req, 0x05, 0x01, 0x00, 0x04)
		req = append(req, ip6...)
	} else {
		if len(host) > 255 {
			return nil, fmt.Errorf("domain too long")
		}
		req = append(req, 0x05, 0x01, 0x00, 0x03, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := c.Write(req); err != nil {
		return nil, err
	}

	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return nil, err
	}
	if hdr[0] != 0x05 {
		return nil, fmt.Errorf("bad socks version")
	}
	if hdr[1] != 0x00 {
		return nil, fmt.Errorf("socks connect failed, rep=%d", hdr[1])
	}

	switch hdr[3] {
	case 0x01:
		_, err = io.ReadFull(c, make([]byte, 4+2))
	case 0x04:
		_, err = io.ReadFull(c, make([]byte, 16+2))
	case 0x03:
		var ln [1]byte
		if _, err = io.ReadFull(c, ln[:]); err != nil {
			return nil, err
		}
		_, err = io.ReadFull(c, make([]byte, int(ln[0])+2))
	default:
		return nil, fmt.Errorf("bad atyp=%d", hdr[3])
	}
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	cc := c
	c = nil
	return cc, nil
}

func parseKey(key string) ([]byte, error) {
	v := strings.TrimSpace(key)
	if v == "" {
		return nil, nil
	}
	if strings.HasPrefix(v, "base64:") {
		return base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "base64:"))
	}
	if strings.HasPrefix(v, "hex:") {
		return hex.DecodeString(strings.TrimPrefix(v, "hex:"))
	}
	if raw, err := base64.StdEncoding.DecodeString(v); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(v); err == nil {
		return raw, nil
	}
	if raw, err := hex.DecodeString(v); err == nil {
		return raw, nil
	}
	return []byte(v), nil
}
