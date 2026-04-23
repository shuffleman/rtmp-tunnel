package main

import (
	"encoding/base64"
	"encoding/hex"
	"flag"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/conn"
)

func main() {
	var (
		listenAddr = flag.String("l", ":19000", "listen address")
		mode       = flag.String("mode", "tcp", "tcp|rtmp")
		appName    = flag.String("app", "live", "app name (rtmp)")
		key        = flag.String("key", "", "encryption key (rtmp), supports base64:/hex: prefix")
		bitrate    = flag.Int("bitrate", 5000000, "target bitrate (rtmp)")
		fps        = flag.Int("fps", 30, "frame rate (rtmp)")
		maxSEI     = flag.Int("maxsei", 1400, "max sei size (rtmp)")
		seiPerF    = flag.Int("seiperframe", 4, "max sei per frame (rtmp)")
		flushMS    = flag.Int("flushms", 5, "flush interval ms (rtmp)")
		batch      = flag.Int("batch", 65536, "write batch size (rtmp)")
		bufSize    = flag.Int("buf", 256*1024, "buffer size")
	)
	flag.Parse()

	ln, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	log.Printf("benchserver started: mode=%s listen=%s", *mode, *listenAddr)

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		go handleConn(c, *mode, *appName, *key, *bufSize, *bitrate, *fps, *maxSEI, *seiPerF, *flushMS, *batch)
	}
}

func handleConn(raw net.Conn, mode, appName, key string, bufSize, bitrate, fps, maxSEI, seiPerFrame, flushMS, batch int) {
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	_ = raw.SetDeadline(time.Time{})

	buf := make([]byte, bufSize)
	switch strings.ToLower(mode) {
	case "tcp":
		_, _ = io.CopyBuffer(raw, raw, buf)
	case "rtmp":
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
				log.Printf("parse key: %v", err)
				return
			}
			cfg.Encryption.Key = k
		} else {
			cfg.Encryption.Key = []byte("test-secret-key")
		}
		cfg.IsServer = true

		rtmpConn := conn.NewServerConn(cfg)
		if err := rtmpConn.Wrap(raw, true); err != nil {
			log.Printf("rtmp wrap: %v", err)
			return
		}
		for {
			n, err := rtmpConn.Read(buf)
			if n > 0 {
				_, werr := rtmpConn.Write(buf[:n])
				if werr != nil {
					log.Printf("rtmp write: %v", werr)
					return
				}
			}
			if err != nil {
				log.Printf("rtmp read: %v", err)
				return
			}
		}
	default:
		log.Printf("unknown mode: %s", mode)
	}
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
