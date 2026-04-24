// conn/loop.go - 客户端/服务端主循环
package conn

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/shuffleman/rtmp-tunnel/command"
	"github.com/shuffleman/rtmp-tunnel/container"
	"github.com/shuffleman/rtmp-tunnel/crypto"
	"github.com/shuffleman/rtmp-tunnel/encoding"
	"github.com/shuffleman/rtmp-tunnel/protocol"
)

// clientLoop 客户端主循环
func (c *RTMPConn) clientLoop() {
	// 启动写循环(将代理数据伪装为视频帧)
	go c.writeLoop()

	// 启动读循环(从视频帧提取代理数据)
	go c.readLoop()

	// 启动帧生成循环(维持码率)
	go c.frameGenerationLoop()

	// 启动控制消息维护
	done := make(chan struct{})
	go c.ctrlMsgs.StartControlMessageLoop(c.tcpConn, done)

	// 等待连接关闭
	<-c.ctx.Done()
	close(done)
}

// serverLoop 服务端主循环
func (c *RTMPConn) serverLoop() {
	// 启动写循环
	go c.writeLoop()

	// 启动读循环
	go c.readLoop()

	// 启动帧生成循环
	go c.frameGenerationLoop()

	// 控制消息维护
	done := make(chan struct{})
	go c.ctrlMsgs.StartControlMessageLoop(c.tcpConn, done)

	// 等待连接关闭
	<-c.ctx.Done()
	close(done)
}

// writeLoop 写循环：从writeCh读取代理数据，编码为H.264视频帧发送
func (c *RTMPConn) writeLoop() {
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()

	var batch bytes.Buffer
	batch.Grow(c.cfg.WriteBatchSize)

	for {
		select {
		case <-c.ctx.Done():
			return

		case data := <-c.writeCh:
			if batch.Len()+len(data) > c.cfg.WriteBatchSize {
				// 加密并分片
				c.encryptAndSend(batch.Bytes())
				batch.Reset()
			}
			batch.Write(data)

		case <-ticker.C:
			if batch.Len() > 0 {
				c.encryptAndSend(batch.Bytes())
				batch.Reset()
			}
		}
	}
}

// readLoop 读循环：从TCP读取RTMP消息，从视频帧提取代理数据
func (c *RTMPConn) readLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		// 读取完整消息
		chunk, payload, err := c.chunkCodec.ReadCompleteMessage(c.tcpConn)
		if err != nil {
			if c.ctx.Err() != nil {
				return // 正常关闭
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			select {
			case c.errCh <- fmt.Errorf("read message: %w", err):
			case <-c.ctx.Done():
			}
			return
		}

		c.bytesRecv.Add(uint64(len(payload)))

		// 处理消息
		if err := c.handleIncomingMessage(chunk, payload); err != nil {
			select {
			case c.errCh <- err:
			case <-c.ctx.Done():
			}
			return
		}
	}
}

// frameGenerationLoop 帧生成循环：定期生成视频/音频帧维持伪装
func (c *RTMPConn) frameGenerationLoop() {
	frameInterval := time.Second / time.Duration(c.cfg.FrameRate)
	ticker := time.NewTicker(frameInterval)
	defer ticker.Stop()

	// 发送初始序列头
	c.sendVideoSequenceHeader()
	c.sendAudioSequenceHeader()

	frameCount := 0
	lastIdleSend := time.Now()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.pendingMu.Lock()
			hasPending := len(c.pendingProxyData) > 0
			c.pendingMu.Unlock()
			if !hasPending && time.Since(lastIdleSend) < time.Second {
				continue
			}
			if !hasPending {
				lastIdleSend = time.Now()
			}
			frameCount++
			// 每N帧发送一次音频标签
			audioEvery := c.cfg.FrameRate / 2
			if audioEvery < 1 {
				audioEvery = 1
			}
			sendAudio := frameCount%audioEvery == 0

			// 生成视频帧
			if err := c.sendVideoFrame(); err != nil {
				return
			}

			if sendAudio {
				if err := c.sendAudioFrame(); err != nil {
					return
				}
			}
		}
	}
}

// handleIncomingMessage 处理接收到的消息
func (c *RTMPConn) handleIncomingMessage(chunk *protocol.Chunk, payload []byte) error {
	switch chunk.MessageTypeID {
	case protocol.MsgTypeSetChunkSize:
		// 处理块大小变更
		msg := &protocol.Message{Type: chunk.MessageTypeID, Payload: payload}
		return c.ctrlMsgs.HandleControlMessage(msg)

	case protocol.MsgTypeWindowAckSize,
		protocol.MsgTypeSetPeerBandwidth,
		protocol.MsgTypeUserControl,
		protocol.MsgTypeAck:
		msg := &protocol.Message{Type: chunk.MessageTypeID, Payload: payload}
		return c.ctrlMsgs.HandleControlMessage(msg)

	case protocol.MsgTypeCommandAMF0:
		return c.handleCommand(payload)

	case protocol.MsgTypeVideo:
		return c.handleVideoMessage(payload)

	case protocol.MsgTypeAudio:
		return c.handleAudioMessage(payload)

	default:
		// 忽略其他消息类型
		return nil
	}
}

// handleCommand 处理命令消息
func (c *RTMPConn) handleCommand(payload []byte) error {
	cmd, err := command.DecodeCommand(payload)
	if err != nil {
		return fmt.Errorf("decode command: %w", err)
	}

	switch cmd.Name {
	case command.CmdResult:
		return c.handleResult(cmd)
	case command.CmdOnStatus:
		return c.handleOnStatus(cmd)
	case command.CmdConnect:
		return c.handleServerConnect(cmd)
	case command.CmdCreateStream:
		return c.handleServerCreateStream(cmd)
	case command.CmdPublish:
		return c.handleServerPublish(cmd)
	case command.CmdPlay:
		return c.handleServerPlay(cmd)
	case command.CmdClose:
		return c.handleServerClose(cmd)
	default:
		return nil
	}
}

// handleResult 处理_result响应
func (c *RTMPConn) handleResult(cmd *command.Command) error {
	// 根据事务ID判断响应类型
	// 简化处理：收到任何_result都认为成功
	return nil
}

// handleOnStatus 处理onStatus通知
func (c *RTMPConn) handleOnStatus(cmd *command.Command) error {
	level, code, _ := command.ParseOnStatus(cmd)
	if level == "error" {
		return fmt.Errorf("onStatus error: %s", code)
	}

	switch code {
	case "NetStream.Publish.Start":
		c.state = StatePublishing
	case "NetStream.Play.Start":
		c.state = StatePlaying
	case "NetStream.Play.Stop",
		"NetStream.Publish.Stop":
		// 流停止
	}
	return nil
}

// handleVideoMessage 处理视频消息 - 提取代理数据
func (c *RTMPConn) handleVideoMessage(payload []byte) error {
	if len(payload) < 2 {
		return nil
	}

	// 解析视频标签
	vd, err := c.flvContainer.ParseVideoTag(payload)
	if err != nil {
		return nil // 非关键错误
	}

	c.framesRecv.Add(1)

	if vd.CodecID != container.VideoCodecAVC {
		return nil
	}

	// 从NAL单元中提取代理数据
	for _, nalu := range vd.NALUs {
		if encoding.IsProxySEI(nalu) {
			data, err := encoding.ExtractSEIPayload(nalu)
			if err == nil && len(data) > 0 {
				if len(data) < crypto.FragmentHeaderSize {
					continue
				}
				header, err := crypto.DecodeFragmentHeader(data[:crypto.FragmentHeaderSize])
				if err != nil {
					continue
				}
				// 尝试重组分片
				fullData, err := c.reassem.AddFragment(data)
				if err == nil && fullData != nil {
					if c.cipher != nil {
						plaintext, derr := c.cipher.Decrypt(fullData)
						if derr == nil {
							fullData = plaintext
						} else {
							return derr
						}
					}
					c.enqueueReceived(header.SequenceNum, fullData)
				}
			}
		}
	}

	return nil
}

// handleAudioMessage 处理音频消息
func (c *RTMPConn) handleAudioMessage(payload []byte) error {
	// 音频通道数据暂不提取代理数据
	return nil
}

// encryptAndSend 加密数据并分片发送
func (c *RTMPConn) encryptAndSend(data []byte) {
	if len(data) == 0 {
		return
	}

	// 加密
	var encrypted []byte
	if c.cipher != nil {
		var err error
		encrypted, err = c.cipher.Encrypt(data)
		if err != nil {
			encrypted = data // 加密失败，发送明文
		}
	} else {
		encrypted = data
	}

	// 分片
	seqNum := c.seqMgr.NextSendSequence()
	frags := c.frag.Fragment(seqNum, encrypted)

	// 标记有代理数据等待嵌入
	c.pendingMu.Lock()
	addBytes := 0
	for _, frag := range frags {
		addBytes += len(frag)
	}
	maxFragLimit := c.cfg.MaxPendingProxyFragments
	if maxFragLimit > 0 && len(frags) > maxFragLimit {
		maxFragLimit = 0
	}
	maxBytesLimit := c.cfg.MaxPendingProxyBytes
	if maxBytesLimit > 0 && addBytes > maxBytesLimit {
		maxBytesLimit = 0
	}
	for c.ctx.Err() == nil {
		if maxFragLimit > 0 && len(c.pendingProxyData)+len(frags) > maxFragLimit {
			c.pendingCond.Wait()
			continue
		}
		if maxBytesLimit > 0 && c.pendingBytes+addBytes > maxBytesLimit {
			c.pendingCond.Wait()
			continue
		}
		break
	}
	if c.ctx.Err() != nil {
		c.pendingMu.Unlock()
		return
	}
	for _, frag := range frags {
		c.pendingProxyData = append(c.pendingProxyData, frag)
	}
	c.pendingBytes += addBytes
	c.pendingMu.Unlock()
}

// getPendingProxyData 获取待嵌入视频帧的代理数据
func (c *RTMPConn) getPendingProxyData() []byte {
	list := c.getPendingProxyDataBatch(1)
	if len(list) == 0 {
		return nil
	}
	return list[0]
}

func (c *RTMPConn) getPendingProxyDataBatch(max int) [][]byte {
	if max <= 0 {
		return nil
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if len(c.pendingProxyData) == 0 {
		return nil
	}
	if max > len(c.pendingProxyData) {
		max = len(c.pendingProxyData)
	}
	data := c.pendingProxyData[:max]
	c.pendingProxyData = c.pendingProxyData[max:]
	removedBytes := 0
	for _, d := range data {
		removedBytes += len(d)
	}
	c.pendingBytes -= removedBytes
	if c.pendingBytes < 0 {
		c.pendingBytes = 0
	}
	if len(c.pendingProxyData) == 0 {
		c.pendingProxyData = nil
	}
	c.pendingCond.Broadcast()
	out := make([][]byte, len(data))
	copy(out, data)
	return out
}

// sendVideoFrame 发送单个视频帧
func (c *RTMPConn) sendVideoFrame() error {
	// 获取待发送的代理数据
	maxPerFrame := c.cfg.MaxSEIPerFrame
	if maxPerFrame <= 0 {
		maxPerFrame = 1
	}
	proxyDataList := c.getPendingProxyDataBatch(maxPerFrame)

	// 生成帧NAL单元
	_, ts, nalus, err := c.gopSim.NextFrameMulti(proxyDataList)
	if err != nil {
		return err
	}

	// 构建视频标签
	var frameType uint8 = container.FrameTypeInterFrame
	if c.gopSim.FrameNum()%uint32(c.cfg.GOPSize) == 0 {
		frameType = container.FrameTypeKeyFrame
	}

	tag, err := c.flvContainer.BuildVideoTag(ts, frameType, container.AVCNALU, 0, nalus)
	if err != nil {
		return err
	}

	// 发送RTMP消息
	payload := container.TagToRTMPMessage(tag)
	csID := uint32(4) // 视频块流
	if err := c.chunkCodec.WriteChunk(c.tcpConn, csID, container.TagTypeVideo, 1, ts, payload); err != nil {
		return err
	}

	c.framesSent.Add(1)
	c.bytesSent.Add(uint64(len(payload)))
	return nil
}

// sendAudioFrame 发送音频帧
func (c *RTMPConn) sendAudioFrame() error {
	// 发送最小AAC静音帧
	aacFrame := encoding.GenerateAACSilentFrame()

	ts := uint32(time.Since(c.startTime).Milliseconds())
	tag, err := c.flvContainer.BuildAudioTag(ts, container.AACRaw, aacFrame)
	if err != nil {
		return err
	}

	payload := container.TagToRTMPMessage(tag)
	csID := uint32(5) // 音频块流
	return c.chunkCodec.WriteChunk(c.tcpConn, csID, container.TagTypeAudio, 1, ts, payload)
}

// sendVideoSequenceHeader 发送视频序列头
func (c *RTMPConn) sendVideoSequenceHeader() error {
	nalus := c.gopSim.BuildSequenceHeader()
	ts := uint32(0)

	tag, err := c.flvContainer.BuildVideoTag(ts, container.FrameTypeKeyFrame, container.AVCSequenceHeader, 0, nalus)
	if err != nil {
		return err
	}

	payload := container.TagToRTMPMessage(tag)
	csID := uint32(4)
	return c.chunkCodec.WriteChunk(c.tcpConn, csID, container.TagTypeVideo, 1, ts, payload)
}

// sendAudioSequenceHeader 发送音频序列头
func (c *RTMPConn) sendAudioSequenceHeader() error {
	aacConfig := encoding.BuildAACAudioSpecificConfig()
	ts := uint32(0)

	tag, err := c.flvContainer.BuildAudioTag(ts, container.AACSequenceHeader, aacConfig)
	if err != nil {
		return err
	}

	payload := container.TagToRTMPMessage(tag)
	csID := uint32(5)
	return c.chunkCodec.WriteChunk(c.tcpConn, csID, container.TagTypeAudio, 1, ts, payload)
}

// === 服务端命令处理器 ===

func (c *RTMPConn) handleServerConnect(cmd *command.Command) error {
	// 解析connect参数
	params := command.ParseConnectCommand(cmd)
	appName := command.GetAppName(params)
	_ = appName

	// 发送connect响应
	resp := command.BuildConnectResult(cmd.TransactionID)
	msg, _ := resp.ToMessage(3)
	return c.chunkCodec.WriteChunk(c.tcpConn, msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload)
}

func (c *RTMPConn) handleServerCreateStream(cmd *command.Command) error {
	c.streamID++
	resp := command.BuildCreateStreamResult(cmd.TransactionID, c.streamID)
	msg, _ := resp.ToMessage(3)
	return c.chunkCodec.WriteChunk(c.tcpConn, msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload)
}

func (c *RTMPConn) handleServerPublish(cmd *command.Command) error {
	// 发送onStatus响应
	status := command.BuildPublishStatus(c.streamID, true)
	msg, _ := status.ToMessage(3)
	c.chunkCodec.WriteChunk(c.tcpConn, msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload)

	// 发送StreamBegin
	c.ctrlMsgs.SendStreamBegin(c.tcpConn, uint32(c.streamID))
	return nil
}

func (c *RTMPConn) handleServerPlay(cmd *command.Command) error {
	// 发送onStatus响应
	status := command.BuildPlayStatus(c.streamID)
	msg, _ := status.ToMessage(3)
	c.chunkCodec.WriteChunk(c.tcpConn, msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload)

	// 发送StreamBegin
	c.ctrlMsgs.SendStreamBegin(c.tcpConn, uint32(c.streamID))
	return nil
}

func (c *RTMPConn) handleServerClose(cmd *command.Command) error {
	c.Close()
	return nil
}
