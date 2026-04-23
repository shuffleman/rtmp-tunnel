// adapter/xray.go - xray-core传输层适配
// 此文件提供xray-core所需的传输层接口适配
package adapter

import (
	"context"
	"fmt"
	"net"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/conn"
)

// XRayAdapter xray-core传输层适配器
type XRayAdapter struct {
	cfg *config.Config
}

// NewXRayAdapter 创建xray适配器
func NewXRayAdapter(cfg *config.Config) *XRayAdapter {
	return &XRayAdapter{cfg: cfg}
}

// Dial 实现xray传输层Dial接口
// 返回net.Conn兼容的连接对象
func (a *XRayAdapter) Dial(ctx context.Context, dest string) (net.Conn, error) {
	rtmpConn := conn.NewClientConn(a.cfg)

	// 支持context超时
	done := make(chan error, 1)
	go func() {
		done <- rtmpConn.Dial(dest)
	}()

	select {
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("rtmp dial: %w", err)
		}
		return rtmpConn, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Listen 实现xray传输层Listen接口
func (a *XRayAdapter) Listen(laddr string) (net.Listener, error) {
	return NewRTMPListener(laddr, a.cfg)
}

// RTMPListener RTMP伪装监听器
type RTMPListener struct {
	listener net.Listener
	cfg      *config.Config
}

// NewRTMPListener 创建RTMP监听器
func NewRTMPListener(laddr string, cfg *config.Config) (*RTMPListener, error) {
	tcpListener, err := net.Listen("tcp", laddr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	return &RTMPListener{
		listener: tcpListener,
		cfg:      cfg,
	}, nil
}

// Accept 接受新连接
func (l *RTMPListener) Accept() (net.Conn, error) {
	rtmpConn := conn.NewServerConn(l.cfg)
	if err := rtmpConn.Accept(l.listener); err != nil {
		return nil, err
	}
	return rtmpConn, nil
}

// Close 关闭监听器
func (l *RTMPListener) Close() error {
	return l.listener.Close()
}

// Addr 返回监听地址
func (l *RTMPListener) Addr() net.Addr {
	return l.listener.Addr()
}

// XRayConfig xray-core配置结构
// 用于xray配置文件中解析RTMP传输层参数
type XRayConfig struct {
	// 传输层通用配置
	AppName            string `json:"appName" yaml:"appName"`
	PublishStreamName  string `json:"publishStreamName" yaml:"publishStreamName"`
	PlaybackStreamName string `json:"playbackStreamName" yaml:"playbackStreamName"`
	TargetBitrate      int    `json:"targetBitrate" yaml:"targetBitrate"`
	FrameRate          int    `json:"frameRate" yaml:"frameRate"`
	GOPSize            int    `json:"gopSize" yaml:"gopSize"`

	// 加密配置
	Encryption struct {
		Enable    bool   `json:"enable" yaml:"enable"`
		Algorithm string `json:"algorithm" yaml:"algorithm"`
		Key       string `json:"key" yaml:"key"`
	} `json:"encryption" yaml:"encryption"`

	// TCP配置
	TCP struct {
		NoDelay         bool `json:"noDelay" yaml:"noDelay"`
		ReadBufferSize  int  `json:"readBufferSize" yaml:"readBufferSize"`
		WriteBufferSize int  `json:"writeBufferSize" yaml:"writeBufferSize"`
		KeepAlive       bool `json:"keepAlive" yaml:"keepAlive"`
	} `json:"tcp" yaml:"tcp"`
}

// ToConfig 转换为内部配置
func (xc *XRayConfig) ToConfig() *config.Config {
	cfg := config.DefaultConfig()

	if xc.AppName != "" {
		cfg.AppName = xc.AppName
	}
	if xc.PublishStreamName != "" {
		cfg.PublishStreamName = xc.PublishStreamName
	}
	if xc.PlaybackStreamName != "" {
		cfg.PlaybackStreamName = xc.PlaybackStreamName
	}
	if xc.TargetBitrate > 0 {
		cfg.TargetBitrate = xc.TargetBitrate
	}
	if xc.FrameRate > 0 {
		cfg.FrameRate = xc.FrameRate
	}
	if xc.GOPSize > 0 {
		cfg.GOPSize = xc.GOPSize
	}

	cfg.Encryption.Enable = xc.Encryption.Enable
	if xc.Encryption.Algorithm != "" {
		cfg.Encryption.Algorithm = xc.Encryption.Algorithm
	}
	if xc.Encryption.Key != "" {
		cfg.Encryption.Key = []byte(xc.Encryption.Key)
	}

	cfg.TCP.NoDelay = xc.TCP.NoDelay
	if xc.TCP.ReadBufferSize > 0 {
		cfg.TCP.ReadBufferSize = xc.TCP.ReadBufferSize
	}
	if xc.TCP.WriteBufferSize > 0 {
		cfg.TCP.WriteBufferSize = xc.TCP.WriteBufferSize
	}
	cfg.TCP.KeepAlive = xc.TCP.KeepAlive

	cfg.Validate()
	return cfg
}

// XRayConfigExample 返回xray配置示例
func XRayConfigExample() string {
	return `# RTMP传输层配置示例 (xray-core)
{
  "transport": "rtmp",
  "rtmpSettings": {
    "appName": "live",
    "publishStreamName": "stream_pub",
    "playbackStreamName": "stream_play",
    "targetBitrate": 5000000,
    "frameRate": 30,
    "gopSize": 60,
    "encryption": {
      "enable": true,
      "algorithm": "aes-256-gcm",
      "key": "your-secret-key-here"
    },
    "tcp": {
      "noDelay": true,
      "readBufferSize": 1048576,
      "writeBufferSize": 1048576,
      "keepAlive": true
    }
  }
}
`
}
