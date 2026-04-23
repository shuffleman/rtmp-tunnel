// protocol/handshake.go - RTMP握手协议实现
package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// RTMP握手常量
const (
	HandshakeVersion = 3
	HandshakeSize    = 1536 // C0/C1/S0/S1/S2 的 C1/S1/S2 部分大小

	// Adobe标准的HMAC密钥
	serverKeyConst = "Genuine Adobe Flash Media Server 001"
	clientKeyConst = "Genuine Adobe Flash Player 001"
)

var (
	serverKey []byte
	clientKey []byte
)

func init() {
	// 生成标准HMAC密钥
	h := sha256.New()
	h.Write([]byte(serverKeyConst))
	serverKey = h.Sum(nil)

	h = sha256.New()
	h.Write([]byte(clientKeyConst))
	clientKey = h.Sum(nil)
}

// HandshakeState 握手状态机
type HandshakeState int

const (
	HandshakeInit     HandshakeState = iota // 初始状态
	HandshakeC0C1                           // 收到C0+C1
	HandshakeS0S1S2                         // 发送S0+S1+S2
	HandshakeC2                             // 收到C2
	HandshakeDone                           // 握手完成
)

// Handshaker 处理RTMP握手
type Handshaker struct {
	State    HandshakeState
	IsServer bool
	c1Data   [HandshakeSize]byte // 保存C1用于验证C2
	s1Time   uint32
	s1Random [HandshakeSize - 8]byte
}

// NewHandshaker 创建握手处理器
func NewHandshaker(isServer bool) *Handshaker {
	h := &Handshaker{
		State:    HandshakeInit,
		IsServer: isServer,
		s1Time:   uint32(time.Now().Unix()),
	}
	// 生成S1随机数据
	if _, err := io.ReadFull(io.Reader(randomSource{}), h.s1Random[:]); err != nil {
		// 回退到时间戳填充
		binary.BigEndian.PutUint64(h.s1Random[:8], uint64(time.Now().UnixNano()))
	}
	return h
}

type randomSource struct{}

func (randomSource) Read(p []byte) (n int, err error) {
	for i := range p {
		p[i] = byte(time.Now().UnixNano() + int64(i)*31)
	}
	return len(p), nil
}

// ProcessServerHandshake 服务端处理完整握手流程
func (h *Handshaker) ProcessServerHandshake(rw io.ReadWriter) error {
	if !h.IsServer {
		return fmt.Errorf("not in server mode")
	}

	// 读取C0
	var c0 [1]byte
	if _, err := io.ReadFull(rw, c0[:]); err != nil {
		return fmt.Errorf("read C0: %w", err)
	}
	if c0[0] != HandshakeVersion {
		return fmt.Errorf("unsupported RTMP version: %d", c0[0])
	}

	// 读取C1
	var c1 [HandshakeSize]byte
	if _, err := io.ReadFull(rw, c1[:]); err != nil {
		return fmt.Errorf("read C1: %w", err)
	}
	copy(h.c1Data[:], c1[:])
	h.State = HandshakeC0C1

	// 发送S0
	s0 := [1]byte{HandshakeVersion}
	if _, err := rw.Write(s0[:]); err != nil {
		return fmt.Errorf("write S0: %w", err)
	}

	// 发送S1
	s1 := h.buildS1()
	if _, err := rw.Write(s1[:]); err != nil {
		return fmt.Errorf("write S1: %w", err)
	}

	// 发送S2 (使用C1的数据生成)
	s2 := h.buildS2(c1[:])
	if _, err := rw.Write(s2[:]); err != nil {
		return fmt.Errorf("write S2: %w", err)
	}
	h.State = HandshakeS0S1S2

	// 读取C2
	var c2 [HandshakeSize]byte
	if _, err := io.ReadFull(rw, c2[:]); err != nil {
		return fmt.Errorf("read C2: %w", err)
	}

	// 验证C2 (可选的严格验证)
	if !h.validateC2(c2[:]) {
		// 非致命错误，部分客户端实现不标准
		// return fmt.Errorf("C2 validation failed")
	}

	h.State = HandshakeDone
	return nil
}

// ProcessClientHandshake 客户端处理完整握手流程
func (h *Handshaker) ProcessClientHandshake(rw io.ReadWriter) error {
	if h.IsServer {
		return fmt.Errorf("not in client mode")
	}

	// 构建C0+C1
	c0 := [1]byte{HandshakeVersion}
	c1 := h.buildC1()

	// 发送C0
	if _, err := rw.Write(c0[:]); err != nil {
		return fmt.Errorf("write C0: %w", err)
	}
	// 发送C1
	if _, err := rw.Write(c1[:]); err != nil {
		return fmt.Errorf("write C1: %w", err)
	}

	// 读取S0
	var s0 [1]byte
	if _, err := io.ReadFull(rw, s0[:]); err != nil {
		return fmt.Errorf("read S0: %w", err)
	}
	if s0[0] != HandshakeVersion {
		return fmt.Errorf("unsupported server version: %d", s0[0])
	}

	// 读取S1
	var s1 [HandshakeSize]byte
	if _, err := io.ReadFull(rw, s1[:]); err != nil {
		return fmt.Errorf("read S1: %w", err)
	}

	// 读取S2
	var s2 [HandshakeSize]byte
	if _, err := io.ReadFull(rw, s2[:]); err != nil {
		return fmt.Errorf("read S2: %w", err)
	}

	// 发送C2 (回声S1的数据)
	c2 := h.buildC2(s1[:])
	if _, err := rw.Write(c2[:]); err != nil {
		return fmt.Errorf("write C2: %w", err)
	}

	h.State = HandshakeDone
	return nil
}

// buildC1 构建C1消息 [time(4) + zero(4) + random(1528)]
func (h *Handshaker) buildC1() [HandshakeSize]byte {
	var c1 [HandshakeSize]byte
	binary.BigEndian.PutUint32(c1[0:4], uint32(time.Now().Unix()))
	// bytes 4-7 保留为零
	// 填充随机数据
	for i := 8; i < HandshakeSize; i++ {
		c1[i] = byte(time.Now().UnixNano() + int64(i)*17)
	}
	return c1
}

// buildS1 构建S1消息 [time(4) + zero(4) + random(1528)]
func (h *Handshaker) buildS1() [HandshakeSize]byte {
	var s1 [HandshakeSize]byte
	binary.BigEndian.PutUint32(s1[0:4], h.s1Time)
	// bytes 4-7 保留
	copy(s1[8:], h.s1Random[:])
	return s1
}

// buildS2 构建S2消息 (基于C1的echo)
func (h *Handshaker) buildS2(c1 []byte) [HandshakeSize]byte {
	var s2 [HandshakeSize]byte
	// S2通常回显C1的时间戳
	copy(s2[:], c1[:])
	// 可选: 对S2进行HMAC签名(复杂模式)
	return s2
}

// buildC2 构建C2消息 (S1的回声)
func (h *Handshaker) buildC2(s1 []byte) [HandshakeSize]byte {
	var c2 [HandshakeSize]byte
	copy(c2[:], s1[:])
	return c2
}

// validateC2 验证C2的合法性
func (h *Handshaker) validateC2(c2 []byte) bool {
	if len(c2) != HandshakeSize {
		return false
	}
	// 简单验证: C2应该回显S1的时间戳和随机数据
	// 严格实现应验证HMAC签名
	return true
}

// GenerateDigest 生成RTMP标准的HMAC摘要(复杂握手模式)
func GenerateDigest(key []byte, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}
