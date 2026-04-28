// conn/conn.go - 连接抽象模块：对外暴露标准网络连接接口
package conn

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shuffleman/rtmp-tunnel/command"
	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/container"
	"github.com/shuffleman/rtmp-tunnel/crypto"
	"github.com/shuffleman/rtmp-tunnel/encoding"
	"github.com/shuffleman/rtmp-tunnel/protocol"
)

// RTMPConn RTMP伪装连接
type RTMPConn struct {
	cfg *config.Config

	// 底层TCP连接
	tcpConn    net.Conn
	mu         sync.RWMutex
	tcpWriteMu sync.Mutex
	startTime  time.Time

	// RTMP协议组件
	handshaker *protocol.Handshaker
	chunkCodec *protocol.ChunkCodec
	ctrlMsgs   *protocol.ControlMessages
	tidGen     *command.TransactionID

	// 会话状态
	state        ConnState
	isServer     bool
	publishCSID  uint32
	playbackCSID uint32
	streamID     float64

	// 容器/编码
	flvContainer *container.FLVContainer
	gopSim       *encoding.GOPSimulator

	// 加密
	cipher  crypto.Cipher
	frag    *crypto.Fragmenter
	reassem *crypto.Reassembler
	seqMgr  *crypto.SequenceManager

	// 读写管道
	readBuf []byte      // 从H.264提取的代理数据缓冲区
	readCh  chan []byte // 读取通道
	writeCh chan []byte // 写入通道
	errCh   chan error  // 错误通道

	readDeadlineUnixNano  atomic.Int64
	writeDeadlineUnixNano atomic.Int64

	recvMu           sync.Mutex
	recvNextSeq      uint32
	recvPackets      map[uint32][]byte
	recvPendingBytes int

	// 控制
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closeErr  error

	scheduler  *proxyScheduler
	schedNotify chan struct{}

	writeBufPool     sync.Pool
	videoPayloadPool sync.Pool

	// 统计
	bytesSent  atomic.Uint64
	bytesRecv  atomic.Uint64
	framesSent atomic.Uint32
	framesRecv atomic.Uint32
}

// ConnState 连接状态
type ConnState int32

const (
	StateInit        ConnState = iota // 初始
	StateHandshaking                  // 握手阶段
	StateConnecting                   // connect命令阶段
	StateStreamSetup                  // 流创建阶段
	StatePublishing                   // 发布流(上行)建立
	StatePlaying                      // 播放流(下行)建立
	StateStreaming                    // 双向数据流传输
	StateClosing                      // 关闭中
	StateClosed                       // 已关闭
)

// NewClientConn 创建客户端连接
func NewClientConn(cfg *config.Config) *RTMPConn {
	return newConn(cfg, false)
}

// NewServerConn 创建服务端连接
func NewServerConn(cfg *config.Config) *RTMPConn {
	return newConn(cfg, true)
}

func newConn(cfg *config.Config, isServer bool) *RTMPConn {
	ctx, cancel := context.WithCancel(context.Background())

	var cipher crypto.Cipher
	if cfg.Encryption.Enable && len(cfg.Encryption.Key) > 0 {
		var err error
		cipher, err = crypto.NewCipher(cfg.Encryption.Algorithm, cfg.Encryption.Key)
		if err != nil {
			// 回退到XOR
			cipher = crypto.NewXORCipher(cfg.Encryption.Key)
		}
	}

	c := &RTMPConn{
		cfg:          cfg,
		isServer:     isServer,
		ctx:          ctx,
		cancel:       cancel,
		startTime:    time.Now(),
		readCh:       make(chan []byte, 64),
		writeCh:      make(chan []byte, 64),
		errCh:        make(chan error, 1),
		cipher:       cipher,
		frag:         crypto.NewFragmenter(cfg.MaxSEISize - 16 - 11), // 减去SEI开销和分片头
		reassem:      crypto.NewReassembler(30 * time.Second),
		seqMgr:       crypto.NewSequenceManager(),
		flvContainer: container.NewFLVContainer(),
		gopSim:       encoding.NewGOPSimulator(cfg),
		tidGen:       command.NewTransactionID(),
		recvNextSeq:  1,
		recvPackets:  make(map[uint32][]byte),
		schedNotify:  make(chan struct{}, 1),
	}
	c.reassem.SetLimits(cfg.MaxReassemblyPackets, cfg.MaxReassemblyBytes)
	c.writeBufPool.New = func() any {
		return make([]byte, 0, 64*1024)
	}
	c.videoPayloadPool.New = func() any {
		return make([]byte, 0, 4*1024)
	}
	c.scheduler = newProxyScheduler(cfg.MaxPendingProxyBytes, cfg.MaxPendingProxyFragments)
	c.scheduler.setHooks(func() {
		select {
		case c.schedNotify <- struct{}{}:
		default:
		}
	}, func(b []byte) {
		c.putWriteBuf(b)
	})
	return c
}

// Dial 客户端拨号
func (c *RTMPConn) Dial(addr string) error {
	tcpConn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	if err := c.configureTCP(tcpConn); err != nil {
		tcpConn.Close()
		return err
	}
	c.tcpConn = &writeLockedConn{Conn: tcpConn, mu: &c.tcpWriteMu, writeTimeout: c.cfg.NetworkWriteTimeout}
	c.isServer = false

	// 执行客户端握手
	c.handshaker = protocol.NewHandshaker(false)
	if err := c.handshaker.ProcessClientHandshake(c.tcpConn); err != nil {
		tcpConn.Close()
		return fmt.Errorf("handshake: %w", err)
	}

	c.chunkCodec = protocol.NewChunkCodec()
	c.chunkCodec.SetMaxMessageSize(c.cfg.MaxRTMPMessageSize)
	c.ctrlMsgs = protocol.NewControlMessages(c.chunkCodec)

	// 发送connect命令
	if err := c.sendConnect(); err != nil {
		tcpConn.Close()
		return fmt.Errorf("connect: %w", err)
	}

	// 启动读写循环
	go c.clientLoop()

	c.state = StateStreaming
	return nil
}

// Accept 服务端接受连接
func (c *RTMPConn) Accept(listener net.Listener) error {
	tcpConn, err := listener.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	if err := c.configureTCP(tcpConn); err != nil {
		tcpConn.Close()
		return err
	}
	c.tcpConn = &writeLockedConn{Conn: tcpConn, mu: &c.tcpWriteMu, writeTimeout: c.cfg.NetworkWriteTimeout}
	c.isServer = true

	// 执行服务端握手
	c.handshaker = protocol.NewHandshaker(true)
	if err := c.handshaker.ProcessServerHandshake(c.tcpConn); err != nil {
		tcpConn.Close()
		return fmt.Errorf("handshake: %w", err)
	}

	c.chunkCodec = protocol.NewChunkCodec()
	c.chunkCodec.SetMaxMessageSize(c.cfg.MaxRTMPMessageSize)
	c.ctrlMsgs = protocol.NewControlMessages(c.chunkCodec)

	// 启动服务端处理循环
	go c.serverLoop()

	c.state = StateStreaming
	return nil
}

// Wrap 包装已有TCP连接
func (c *RTMPConn) Wrap(tcpConn net.Conn, isServer bool) error {
	if err := c.configureTCP(tcpConn); err != nil {
		return err
	}
	c.tcpConn = &writeLockedConn{Conn: tcpConn, mu: &c.tcpWriteMu, writeTimeout: c.cfg.NetworkWriteTimeout}
	c.isServer = isServer
	c.handshaker = protocol.NewHandshaker(isServer)

	if isServer {
		if err := c.handshaker.ProcessServerHandshake(c.tcpConn); err != nil {
			return fmt.Errorf("handshake: %w", err)
		}
	} else {
		if err := c.handshaker.ProcessClientHandshake(c.tcpConn); err != nil {
			return fmt.Errorf("handshake: %w", err)
		}
	}

	c.chunkCodec = protocol.NewChunkCodec()
	c.chunkCodec.SetMaxMessageSize(c.cfg.MaxRTMPMessageSize)
	c.ctrlMsgs = protocol.NewControlMessages(c.chunkCodec)

	if err := c.ctrlMsgs.SendInitialControlMessages(c.tcpConn, c.cfg.MaxChunkSize); err != nil {
		return fmt.Errorf("initial control: %w", err)
	}

	if !isServer {
		if err := c.sendConnect(); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
	}

	if isServer {
		go c.serverLoop()
	} else {
		go c.clientLoop()
	}

	c.state = StateStreaming
	return nil
}

// Read 实现io.Reader接口 - 读取代理数据
func (c *RTMPConn) Read(p []byte) (n int, err error) {
	c.mu.Lock()
	if len(c.readBuf) > 0 {
		n = copy(p, c.readBuf)
		c.readBuf = c.readBuf[n:]
		if len(c.readBuf) == 0 {
			c.readBuf = nil
		}
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()

	if deadline := c.readDeadlineUnixNano.Load(); deadline > 0 {
		now := time.Now().UnixNano()
		if now >= deadline {
			return 0, timeoutError{}
		}
		timer := time.NewTimer(time.Until(time.Unix(0, deadline)))
		defer timer.Stop()
		select {
		case data, ok := <-c.readCh:
			if !ok {
				return 0, io.EOF
			}
			n = copy(p, data)
			if n < len(data) {
				c.mu.Lock()
				c.readBuf = append(c.readBuf[:0], data[n:]...)
				c.mu.Unlock()
			}
			return n, nil
		case err := <-c.errCh:
			return 0, err
		case <-c.ctx.Done():
			return 0, io.EOF
		case <-timer.C:
			return 0, timeoutError{}
		}
	}

	select {
	case data, ok := <-c.readCh:
		if !ok {
			return 0, io.EOF
		}
		n = copy(p, data)
		if n < len(data) {
			c.mu.Lock()
			c.readBuf = append(c.readBuf[:0], data[n:]...)
			c.mu.Unlock()
		}
		return n, nil
	case err := <-c.errCh:
		return 0, err
	case <-c.ctx.Done():
		return 0, io.EOF
	}
}

// Write 实现io.Writer接口 - 写入代理数据
func (c *RTMPConn) Write(p []byte) (n int, err error) {
	select {
	case <-c.ctx.Done():
		return 0, fmt.Errorf("connection closed")
	default:
	}

	data := c.getWriteBuf(len(p))
	copy(data, p)

	if deadline := c.writeDeadlineUnixNano.Load(); deadline > 0 {
		now := time.Now().UnixNano()
		if now >= deadline {
			c.putWriteBuf(data)
			return 0, timeoutError{}
		}
		timer := time.NewTimer(time.Until(time.Unix(0, deadline)))
		defer timer.Stop()
		select {
		case c.writeCh <- data:
			return len(p), nil
		case <-c.ctx.Done():
			c.putWriteBuf(data)
			return 0, fmt.Errorf("connection closed")
		case <-timer.C:
			c.putWriteBuf(data)
			return 0, timeoutError{}
		}
	}

	select {
	case c.writeCh <- data:
		return len(p), nil
	case <-c.ctx.Done():
		c.putWriteBuf(data)
		return 0, fmt.Errorf("connection closed")
	}
}

func (c *RTMPConn) getWriteBuf(n int) []byte {
	if n <= 0 {
		return nil
	}
	if n > 64*1024 {
		return make([]byte, n)
	}
	v := c.writeBufPool.Get()
	if v == nil {
		return make([]byte, n)
	}
	b := v.([]byte)
	if cap(b) < n {
		c.writeBufPool.Put(b[:0])
		return make([]byte, n)
	}
	return b[:n]
}

func (c *RTMPConn) putWriteBuf(b []byte) {
	if cap(b) == 0 || cap(b) > 64*1024 {
		return
	}
	c.writeBufPool.Put(b[:0])
}

func (c *RTMPConn) getVideoPayloadBuf() []byte {
	v := c.videoPayloadPool.Get()
	if v == nil {
		return nil
	}
	return v.([]byte)[:0]
}

func (c *RTMPConn) putVideoPayloadBuf(b []byte) {
	if cap(b) == 0 || cap(b) > 2*1024*1024 {
		return
	}
	c.videoPayloadPool.Put(b[:0])
}

// Close 实现io.Closer接口
func (c *RTMPConn) Close() error {
	c.closeOnce.Do(func() {
		c.state = StateClosing
		c.closeErr = fmt.Errorf("connection closed")
		c.cancel()
		if c.scheduler != nil {
			c.scheduler.close()
		}
		if c.tcpConn != nil {
			c.tcpConn.Close()
		}
		c.state = StateClosed
	})
	return nil
}

func (c *RTMPConn) fail(err error) {
	if err != nil {
		select {
		case c.errCh <- err:
		default:
		}
	}
	_ = c.Close()
}

// LocalAddr 返回本地地址
func (c *RTMPConn) LocalAddr() net.Addr {
	if c.tcpConn != nil {
		return c.tcpConn.LocalAddr()
	}
	return nil
}

// RemoteAddr 返回远程地址
func (c *RTMPConn) RemoteAddr() net.Addr {
	if c.tcpConn != nil {
		return c.tcpConn.RemoteAddr()
	}
	return nil
}

// SetDeadline 设置读写截止时间
func (c *RTMPConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// SetReadDeadline 设置读截止时间
func (c *RTMPConn) SetReadDeadline(t time.Time) error {
	if t.IsZero() {
		c.readDeadlineUnixNano.Store(0)
		return nil
	}
	c.readDeadlineUnixNano.Store(t.UnixNano())
	return nil
}

// SetWriteDeadline 设置写截止时间
func (c *RTMPConn) SetWriteDeadline(t time.Time) error {
	if t.IsZero() {
		c.writeDeadlineUnixNano.Store(0)
	} else {
		c.writeDeadlineUnixNano.Store(t.UnixNano())
	}
	return nil
}

// Stats 返回连接统计
func (c *RTMPConn) Stats() (sent, recv uint64, framesSent, framesRecv uint32) {
	return c.bytesSent.Load(), c.bytesRecv.Load(), c.framesSent.Load(), c.framesRecv.Load()
}

// State 返回当前连接状态
func (c *RTMPConn) State() ConnState {
	return ConnState(atomic.LoadInt32((*int32)(&c.state)))
}

func (c *RTMPConn) enqueueReceived(seq uint32, data []byte) {
	c.recvMu.Lock()
	if seq >= c.recvNextSeq {
		if c.cfg.MaxRecvReorderPackets > 0 {
			maxSeq := c.recvNextSeq + uint32(c.cfg.MaxRecvReorderPackets)
			if seq > maxSeq {
				c.recvMu.Unlock()
				return
			}
		}
		if _, exists := c.recvPackets[seq]; !exists {
			if c.cfg.MaxRecvReorderBytes > 0 {
				if c.recvPendingBytes+len(data) > c.cfg.MaxRecvReorderBytes {
					c.recvMu.Unlock()
					return
				}
			}
			c.recvPackets[seq] = data
			c.recvPendingBytes += len(data)
		}
	}
	var toSend [][]byte
	for {
		d, ok := c.recvPackets[c.recvNextSeq]
		if !ok {
			break
		}
		delete(c.recvPackets, c.recvNextSeq)
		c.recvPendingBytes -= len(d)
		c.recvNextSeq++
		toSend = append(toSend, d)
	}
	if len(c.recvPackets) == 0 {
		c.recvPendingBytes = 0
	}
	c.recvMu.Unlock()
	for _, d := range toSend {
		if c.ctx.Err() != nil {
			return
		}
		select {
		case c.readCh <- d:
		case <-c.ctx.Done():
			return
		}
	}
}

// configureTCP 配置TCP参数
func (c *RTMPConn) configureTCP(conn net.Conn) error {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return nil
	}

	if c.cfg.TCP.NoDelay {
		if err := tcpConn.SetNoDelay(true); err != nil {
			return err
		}
	}
	if c.cfg.TCP.ReadBufferSize > 0 {
		if err := tcpConn.SetReadBuffer(c.cfg.TCP.ReadBufferSize); err != nil {
			return err
		}
	}
	if c.cfg.TCP.WriteBufferSize > 0 {
		if err := tcpConn.SetWriteBuffer(c.cfg.TCP.WriteBufferSize); err != nil {
			return err
		}
	}
	if c.cfg.TCP.KeepAlive {
		if err := tcpConn.SetKeepAlive(true); err != nil {
			return err
		}
		if err := tcpConn.SetKeepAlivePeriod(c.cfg.TCP.KeepAlivePeriod); err != nil {
			return err
		}
	}

	return nil
}

// sendConnect 发送connect命令(客户端)
func (c *RTMPConn) sendConnect() error {
	tcURL := fmt.Sprintf("rtmp://%s/%s", c.tcpConn.RemoteAddr().String(), c.cfg.AppName)
	cmd := command.BuildConnect(c.tidGen.Next(), c.cfg.AppName, tcURL)
	msg, err := cmd.ToMessage(3) // chunk stream 3 for commands
	if err != nil {
		return err
	}
	return c.chunkCodec.WriteChunk(c.tcpConn, msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload)
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type writeLockedConn struct {
	net.Conn
	mu           *sync.Mutex
	writeTimeout time.Duration
}

func (c *writeLockedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.writeTimeout > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	n, err := c.Conn.Write(p)
	c.mu.Unlock()
	return n, err
}
