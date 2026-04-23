// testserver - RTMP伪装传输层测试服务端
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/conn"
)

func main() {
	var (
		listenAddr = flag.String("l", ":1935", "监听地址")
		appName    = flag.String("app", "live", "应用名称")
		key        = flag.String("key", "test-secret-key", "加密密钥")
		bitrate    = flag.Int("bitrate", 5000000, "目标码率(bps)")
		frameRate  = flag.Int("fps", 30, "帧率")
		gopSize    = flag.Int("gop", 60, "GOP大小")
	)
	flag.Parse()

	cfg := config.DefaultConfig()
	cfg.AppName = *appName
	cfg.TargetBitrate = *bitrate
	cfg.FrameRate = *frameRate
	cfg.GOPSize = *gopSize
	cfg.Encryption.Enable = true
	cfg.Encryption.Algorithm = "aes-256-gcm"
	cfg.Encryption.Key = []byte(*key)
	cfg.IsServer = true

	// 创建TCP监听器
	tcpListener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	defer tcpListener.Close()

	log.Printf("RTMP伪装传输层测试服务端启动: %s", *listenAddr)
	log.Printf("配置: app=%s, bitrate=%d, fps=%d, gop=%d", *appName, *bitrate, *frameRate, *gopSize)

	// 信号处理
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("正在关闭...")
		tcpListener.Close()
		os.Exit(0)
	}()

	// 接受连接
	for {
		tcpConn, err := tcpListener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok && opErr.Temporary() {
				continue
			}
			log.Printf("接受连接失败: %v", err)
			return
		}

		go handleConnection(tcpConn, cfg)
	}
}

func handleConnection(tcpConn net.Conn, cfg *config.Config) {
	remoteAddr := tcpConn.RemoteAddr().String()
	log.Printf("新连接: %s", remoteAddr)

	rtmpConn := conn.NewServerConn(cfg)
	if err := rtmpConn.Wrap(tcpConn, true); err != nil {
		log.Printf("连接初始化失败 %s: %v", remoteAddr, err)
		return
	}

	// 启动回声测试(将收到的数据返回)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := rtmpConn.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("读取错误 %s: %v", remoteAddr, err)
				}
				return
			}

			// 回声
			if _, err := rtmpConn.Write(buf[:n]); err != nil {
				log.Printf("写入错误 %s: %v", remoteAddr, err)
				return
			}

			sent, recv, framesSent, framesRecv := rtmpConn.Stats()
			log.Printf("[%s] 已收:%d 已发:%d 帧收:%d 帧发:%d",
				remoteAddr, recv, sent, framesRecv, framesSent)
		}
	}()

	// 发送测试数据
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	counter := 0
	for {
		select {
		case <-ticker.C:
			counter++
			msg := fmt.Sprintf("服务端心跳 #%d - %s", counter, time.Now().Format("15:04:05"))
			if _, err := rtmpConn.Write([]byte(msg)); err != nil {
				log.Printf("心跳发送失败 %s: %v", remoteAddr, err)
				return
			}
		}
	}
}
