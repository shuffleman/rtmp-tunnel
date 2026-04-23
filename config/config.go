// config/config.go - RTMP伪装传输层配置定义
package config

import (
	"crypto/tls"
	"time"
)

// Config 定义RTMP伪装传输层的完整配置
type Config struct {
	// 应用标识
	AppName string `json:"appName" yaml:"appName"`

	// 流标识
	PublishStreamName  string `json:"publishStreamName" yaml:"publishStreamName"`  // 上行流名称（如主播推流）
	PlaybackStreamName string `json:"playbackStreamName" yaml:"playbackStreamName"` // 下行流名称（如观众播放）

	// 伪装码率控制
	TargetBitrate int `json:"targetBitrate" yaml:"targetBitrate"` // 目标码率(bps)，默认 5Mbps
	FrameRate     int `json:"frameRate" yaml:"frameRate"`         // 帧率，默认 30fps
	GOPSize       int `json:"gopSize" yaml:"gopSize"`             // GOP长度，默认 60帧(2秒)

	// 块大小
	InitialChunkSize int `json:"initialChunkSize" yaml:"initialChunkSize"` // 初始块大小，默认 128
	MaxChunkSize     int `json:"maxChunkSize" yaml:"maxChunkSize"`         // 协商后最大块大小，默认 4096

	// 加密配置
	Encryption struct {
		Enable     bool   `json:"enable" yaml:"enable"`         // 是否启用内部加密
		Algorithm  string `json:"algorithm" yaml:"algorithm"`   // 加密算法，默认 aes-256-gcm
		Key        []byte `json:"key" yaml:"key"`               // 预共享密钥
		DisableAES bool   `json:"disableAES" yaml:"disableAES"` // 禁用AES，使用XOR（仅用于测试）
	} `json:"encryption" yaml:"encryption"`

	// 性能调优
	WriteBufferSize    int           `json:"writeBufferSize" yaml:"writeBufferSize"`       // 写缓冲区大小
	ReadBufferSize     int           `json:"readBufferSize" yaml:"readBufferSize"`         // 读缓冲区大小
	FlushInterval      time.Duration `json:"flushInterval" yaml:"flushInterval"`           // 写刷新间隔
	WriteBatchSize     int           `json:"writeBatchSize" yaml:"writeBatchSize"`         // 批量写入阈值
	MaxSEISize         int           `json:"maxSEISize" yaml:"maxSEISize"`                 // 单个SEI最大负载
	MaxSEIPerFrame     int           `json:"maxSEIPerFrame" yaml:"maxSEIPerFrame"`         // 每帧最多嵌入的SEI数量
	MaxFramePayloadSize int          `json:"maxFramePayloadSize" yaml:"maxFramePayloadSize"`
	TimestampJitterMax int           `json:"timestampJitterMax" yaml:"timestampJitterMax"` // 时间戳最大抖动(ms)

	// TCP调优
	TCP struct {
		NoDelay         bool          `json:"noDelay" yaml:"noDelay"`
		ReadBufferSize  int           `json:"readBufferSize" yaml:"readBufferSize"`
		WriteBufferSize int           `json:"writeBufferSize" yaml:"writeBufferSize"`
		KeepAlive       bool          `json:"keepAlive" yaml:"keepAlive"`
		KeepAlivePeriod time.Duration `json:"keepAlivePeriod" yaml:"keepAlivePeriod"`
	} `json:"tcp" yaml:"tcp"`

	// TLS配置(可选)
	TLSConfig *tls.Config `json:"-" yaml:"-"`

	// 服务端/客户端模式
	IsServer bool `json:"-" yaml:"-"`
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	c := &Config{
		AppName:            "live",
		PublishStreamName:  "stream_pub",
		PlaybackStreamName: "stream_play",
		TargetBitrate:      5_000_000, // 5 Mbps
		FrameRate:          30,
		GOPSize:            60,
		InitialChunkSize:   128,
		MaxChunkSize:       4096,
		WriteBufferSize:    64 * 1024,
		ReadBufferSize:     64 * 1024,
		FlushInterval:      5 * time.Millisecond,
		WriteBatchSize:     16 * 1024,
		MaxSEISize:         1400, // 小于MTU避免分片
		MaxSEIPerFrame:     4,
		MaxFramePayloadSize: 4096,
		TimestampJitterMax: 5,    // ±5ms抖动
	}
	c.Encryption.Enable = true
	c.Encryption.Algorithm = "aes-256-gcm"
	c.Encryption.DisableAES = false
	c.TCP.NoDelay = true
	c.TCP.ReadBufferSize = 1024 * 1024  // 1MB
	c.TCP.WriteBufferSize = 1024 * 1024 // 1MB
	c.TCP.KeepAlive = true
	c.TCP.KeepAlivePeriod = 30 * time.Second
	return c
}

// Validate 验证配置合法性
func (c *Config) Validate() error {
	if c.TargetBitrate <= 0 {
		c.TargetBitrate = 5_000_000
	}
	if c.FrameRate <= 0 {
		c.FrameRate = 30
	}
	if c.GOPSize <= 0 {
		c.GOPSize = 60
	}
	if c.InitialChunkSize <= 0 {
		c.InitialChunkSize = 128
	}
	if c.MaxChunkSize <= 0 {
		c.MaxChunkSize = 4096
	}
	if c.MaxChunkSize > 65536 {
		c.MaxChunkSize = 65536
	}
	if c.MaxChunkSize < c.InitialChunkSize {
		c.MaxChunkSize = 4096
	}
	if c.MaxSEISize <= 0 {
		c.MaxSEISize = 1400
	}
	if c.MaxSEIPerFrame <= 0 {
		c.MaxSEIPerFrame = 1
	}
	if c.MaxFramePayloadSize < 0 {
		c.MaxFramePayloadSize = 0
	}
	if c.FlushInterval <= 0 {
		c.FlushInterval = 5 * time.Millisecond
	}
	return nil
}
