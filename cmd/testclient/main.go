// testclient - RTMP伪装传输层测试客户端
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/conn"
)

func main() {
	var (
		serverAddr = flag.String("s", "127.0.0.1:1935", "服务端地址")
		appName    = flag.String("app", "live", "应用名称")
		key        = flag.String("key", "test-secret-key", "加密密钥")
		bitrate    = flag.Int("bitrate", 5000000, "目标码率(bps)")
		frameRate  = flag.Int("fps", 30, "帧率")
		gopSize    = flag.Int("gop", 60, "GOP大小")
		duration   = flag.Int("t", 60, "测试持续时间(秒)")
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
	cfg.IsServer = false

	log.Printf("RTMP伪装传输层测试客户端启动")
	log.Printf("目标服务端: %s", *serverAddr)
	log.Printf("配置: app=%s, bitrate=%d, fps=%d, gop=%d, duration=%ds", *appName, *bitrate, *frameRate, *gopSize, *duration)

	// 拨号连接
	rtmpConn := conn.NewClientConn(cfg)
	if err := rtmpConn.Dial(*serverAddr); err != nil {
		log.Fatalf("拨号失败: %v", err)
	}
	defer rtmpConn.Close()

	log.Printf("连接成功: %s -> %s", rtmpConn.LocalAddr(), rtmpConn.RemoteAddr())

	// 信号处理
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// 读取响应
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := rtmpConn.Read(buf)
			if err != nil {
				if err != io.EOF {
					log.Printf("读取错误: %v", err)
				}
				return
			}
			log.Printf("[收到] %s", string(buf[:n]))
		}
	}()

	// 发送测试数据
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		counter := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				counter++
				msg := fmt.Sprintf("客户端消息 #%d - %s", counter, time.Now().Format("15:04:05"))
				if _, err := rtmpConn.Write([]byte(msg)); err != nil {
					log.Printf("发送失败: %v", err)
					return
				}

				sent, recv, framesSent, framesRecv := rtmpConn.Stats()
				log.Printf("[统计] 已发:%d 已收:%d 帧发:%d 帧收:%d",
					sent, recv, framesSent, framesRecv)
			}
		}
	}()

	// 等待超时或信号
	select {
	case <-time.After(time.Duration(*duration) * time.Second):
		log.Printf("测试完成 (%d秒)", *duration)
	case <-sigCh:
		log.Println("收到中断信号")
	}

	close(done)

	// 最终统计
	sent, recv, framesSent, framesRecv := rtmpConn.Stats()
	log.Printf("=== 最终统计 ===")
	log.Printf("发送字节: %d (%.2f MB)", sent, float64(sent)/(1024*1024))
	log.Printf("接收字节: %d (%.2f MB)", recv, float64(recv)/(1024*1024))
	log.Printf("发送帧数: %d", framesSent)
	log.Printf("接收帧数: %d", framesRecv)
}
