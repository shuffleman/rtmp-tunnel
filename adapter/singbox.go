// adapter/singbox.go - sing-box出站/入站适配
// 此文件提供sing-box所需的传输层接口适配
package adapter

import (
	"context"
	"fmt"
	"net"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/conn"
)

// SingBoxAdapter sing-box传输层适配器
type SingBoxAdapter struct {
	cfg *config.Config
}

// NewSingBoxAdapter 创建sing-box适配器
func NewSingBoxAdapter(cfg *config.Config) *SingBoxAdapter {
	return &SingBoxAdapter{cfg: cfg}
}

// DialOutbound 出站拨号
// 建立到服务端的RTMP伪装连接
func (a *SingBoxAdapter) DialOutbound(ctx context.Context, network, address string) (net.Conn, error) {
	rtmpConn := conn.NewClientConn(a.cfg)

	done := make(chan error, 1)
	go func() {
		done <- rtmpConn.Dial(address)
	}()

	select {
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("rtmp outbound dial: %w", err)
		}
		return rtmpConn, nil
	case <-ctx.Done():
		rtmpConn.Close()
		return nil, ctx.Err()
	}
}

// ListenInbound 入站监听
// 在指定端口接受RTMP伪装连接
func (a *SingBoxAdapter) ListenInbound(network, address string) (net.Listener, error) {
	return NewRTMPListener(address, a.cfg)
}

// RTMPInboundConn 入站连接包装器
type RTMPInboundConn struct {
	*conn.RTMPConn
}

// SingBoxConfig sing-box配置结构
type SingBoxConfig struct {
	// 出站配置
	Outbound struct {
		Type   string `json:"type" yaml:"type"`     // "rtmp"
		Server string `json:"server" yaml:"server"` // 服务端地址
		Port   int    `json:"server_port" yaml:"server_port"`

		// RTMP特定配置
		RTMP struct {
			AppName            string `json:"app_name" yaml:"app_name"`
			PublishStreamName  string `json:"publish_stream" yaml:"publish_stream"`
			PlaybackStreamName string `json:"playback_stream" yaml:"playback_stream"`
			TargetBitrate      int    `json:"target_bitrate" yaml:"target_bitrate"`
			FrameRate          int    `json:"frame_rate" yaml:"frame_rate"`
			GOPSize            int    `json:"gop_size" yaml:"gop_size"`

			Encryption struct {
				Enable    bool   `json:"enable" yaml:"enable"`
				Algorithm string `json:"algorithm" yaml:"algorithm"`
				Key       string `json:"key" yaml:"key"`
			} `json:"encryption" yaml:"encryption"`
		} `json:"rtmp" yaml:"rtmp"`
	} `json:"outbound" yaml:"outbound"`

	// 入站配置
	Inbound struct {
		Type   string `json:"type" yaml:"type"` // "rtmp"
		Listen string `json:"listen" yaml:"listen"`
		Port   int    `json:"listen_port" yaml:"listen_port"`

		RTMP struct {
			AppName            string `json:"app_name" yaml:"app_name"`
			PublishStreamName  string `json:"publish_stream" yaml:"publish_stream"`
			PlaybackStreamName string `json:"playback_stream" yaml:"playback_stream"`
			TargetBitrate      int    `json:"target_bitrate" yaml:"target_bitrate"`
			FrameRate          int    `json:"frame_rate" yaml:"frame_rate"`
		} `json:"rtmp" yaml:"rtmp"`
	} `json:"inbound" yaml:"inbound"`
}

// ToConfig 转换为内部配置
func (sc *SingBoxConfig) ToConfig(isServer bool) *config.Config {
	cfg := config.DefaultConfig()
	cfg.IsServer = isServer

	if isServer {
		// 入站配置
		r := sc.Inbound.RTMP
		if r.AppName != "" {
			cfg.AppName = r.AppName
		}
		if r.PublishStreamName != "" {
			cfg.PublishStreamName = r.PublishStreamName
		}
		if r.PlaybackStreamName != "" {
			cfg.PlaybackStreamName = r.PlaybackStreamName
		}
		if r.TargetBitrate > 0 {
			cfg.TargetBitrate = r.TargetBitrate
		}
		if r.FrameRate > 0 {
			cfg.FrameRate = r.FrameRate
		}
	} else {
		// 出站配置
		r := sc.Outbound.RTMP
		if r.AppName != "" {
			cfg.AppName = r.AppName
		}
		if r.PublishStreamName != "" {
			cfg.PublishStreamName = r.PublishStreamName
		}
		if r.PlaybackStreamName != "" {
			cfg.PlaybackStreamName = r.PlaybackStreamName
		}
		if r.TargetBitrate > 0 {
			cfg.TargetBitrate = r.TargetBitrate
		}
		if r.FrameRate > 0 {
			cfg.FrameRate = r.FrameRate
		}
		if r.GOPSize > 0 {
			cfg.GOPSize = r.GOPSize
		}

		cfg.Encryption.Enable = r.Encryption.Enable
		if r.Encryption.Algorithm != "" {
			cfg.Encryption.Algorithm = r.Encryption.Algorithm
		}
		if r.Encryption.Key != "" {
			cfg.Encryption.Key = []byte(r.Encryption.Key)
		}
	}

	cfg.Validate()
	return cfg
}

// SingBoxConfigExample 返回sing-box配置示例
func SingBoxConfigExample() string {
	return `# RTMP传输层配置示例 (sing-box)

# 出站配置
outbounds:
  - type: rtmp
    tag: rtmp-out
    server: your-server.com
    server_port: 1935
    rtmp:
      app_name: live
      publish_stream: stream_pub
      playback_stream: stream_play
      target_bitrate: 5000000
      frame_rate: 30
      gop_size: 60
      encryption:
        enable: true
        algorithm: aes-256-gcm
        key: "your-secret-key-here"

# 入站配置
inbounds:
  - type: rtmp
    tag: rtmp-in
    listen: 0.0.0.0
    listen_port: 1935
    rtmp:
      app_name: live
      publish_stream: stream_pub
      playback_stream: stream_play
      target_bitrate: 5000000
      frame_rate: 30
`
}

// 兼容性类型别名
type (
	// Dialer 出站拨号器接口
	Dialer interface {
		DialContext(ctx context.Context, network, address string) (net.Conn, error)
	}

	// Listener 入站监听器接口
	Listener interface {
		Listen(network, address string) (net.Listener, error)
	}
)

// Ensure interface compliance
var (
	_ Dialer   = (*SingBoxAdapter)(nil)
	_ Listener = (*SingBoxAdapter)(nil)
)

// DialContext 实现标准Dialer接口
func (a *SingBoxAdapter) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return a.DialOutbound(ctx, network, address)
}

// Listen 实现标准Listener接口
func (a *SingBoxAdapter) Listen(network, address string) (net.Listener, error) {
	return a.ListenInbound(network, address)
}

// NewOutbound 创建出站适配器(工厂函数)
func NewOutbound(cfg *config.Config) *SingBoxAdapter {
	return NewSingBoxAdapter(cfg)
}

// NewInbound 创建入站适配器(工厂函数)
func NewInbound(cfg *config.Config) *SingBoxAdapter {
	return NewSingBoxAdapter(cfg)
}
