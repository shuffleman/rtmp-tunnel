// conn/loop.go - 客户端/服务端主循环
package conn

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/shuffleman/rtmp-tunnel/command"
	"github.com/shuffleman/rtmp-tunnel/container"
	"github.com/shuffleman/rtmp-tunnel/crypto"
	"github.com/shuffleman/rtmp-tunnel/encoding"
	"github.com/shuffleman/rtmp-tunnel/metrics"
	"github.com/shuffleman/rtmp-tunnel/protocol"
)

// clientLoop 客户端主循环
func (c *RTMPConn) clientLoop() {
	c.startOutLoop()

	// 启动写循环(将代理数据伪装为视频帧)
	go c.writeLoop()

	// 启动读循环(从视频帧提取代理数据)
	go c.readLoop()

	// 启动帧生成循环(维持码率)
	go c.frameGenerationLoop()

	// 启动控制消息维护
	done := make(chan struct{})
	go c.ctrlMsgs.StartControlMessageLoop(c, done, c.fail)

	// 等待连接关闭
	<-c.ctx.Done()
	close(done)
}

// serverLoop 服务端主循环
func (c *RTMPConn) serverLoop() {
	c.startOutLoop()

	// 启动写循环
	go c.writeLoop()

	// 启动读循环
	go c.readLoop()

	// 启动帧生成循环
	go c.frameGenerationLoop()

	// 控制消息维护
	done := make(chan struct{})
	go c.ctrlMsgs.StartControlMessageLoop(c, done, c.fail)

	// 等待连接关闭
	<-c.ctx.Done()
	close(done)
}

// writeLoop 写循环：从writeCh读取代理数据，编码为H.264视频帧发送
func (c *RTMPConn) writeLoop() {
	ticker := time.NewTicker(c.cfg.FlushInterval)
	defer ticker.Stop()

	var (
		parser     muxFrameParser
		modeRaw    bool
		detectDone bool
		enableXray bool
		reqNeed    int
		holdSince  time.Time
		rawBuf     bytes.Buffer
	)
	rawBuf.Grow(c.cfg.WriteBatchSize)

	peekXray := func(buf []byte) (plausible bool, complete bool) {
		if len(buf) < 2 {
			return false, false
		}
		metaLen := int(binary.BigEndian.Uint16(buf[:2]))
		if metaLen < 4 || metaLen > xrayMuxMaxMetadata {
			return false, false
		}
		if len(buf) < 2+metaLen {
			return true, false
		}
		meta := buf[2 : 2+metaLen]
		status := meta[2]
		option := meta[3]
		if status != 0x01 && status != 0x02 {
			return false, false
		}
		off := 4
		if status == 0x01 {
			if len(meta) < off+1+2 {
				return false, false
			}
			network := meta[off]
			if network != 0x01 && network != 0x02 {
				return false, false
			}
		}
		pos := 2 + metaLen
		if option&0x01 != 0 {
			if len(buf) < pos+2 {
				return true, false
			}
			dlen := int(binary.BigEndian.Uint16(buf[pos : pos+2]))
			pos += 2
			if dlen < 0 || dlen > 65535 {
				return false, false
			}
			if len(buf) < pos+dlen {
				return true, false
			}
		}
		return true, true
	}

	flushRaw := func() {
		if rawBuf.Len() == 0 {
			return
		}
		b := c.getWriteBuf(rawBuf.Len())
		copy(b, rawBuf.Bytes())
		rawBuf.Reset()
		c.scheduler.enqueue(muxFrameMeta{}, b, true, c.seqMgr.NextSendSequence())
	}

	for {
		select {
		case <-c.ctx.Done():
			flushRaw()
			return

		case data := <-c.writeCh:
			if len(data) == 0 {
				c.putWriteBuf(data)
				continue
			}
			if modeRaw {
				if len(data) < smallPacketThreshold {
					b := c.getWriteBuf(len(data))
					copy(b, data)
					c.scheduler.enqueue(muxFrameMeta{}, b, true, c.seqMgr.NextSendSequence())
					c.putWriteBuf(data)
					continue
				}
				if rawBuf.Len()+len(data) > c.cfg.WriteBatchSize {
					flushRaw()
				}
				rawBuf.Write(data)
				c.putWriteBuf(data)
				continue
			}

			parser.push(data)
			c.putWriteBuf(data)
			if holdSince.IsZero() && len(parser.buf) > 0 {
				holdSince = time.Now()
			}
			if !detectDone {
				if plausible, complete := peekXray(parser.buf); plausible {
					if complete {
						detectDone = true
						enableXray = true
						holdSince = time.Time{}
					}
				} else {
					if reqNeed == 0 && len(parser.buf) >= 2 {
						ver := parser.buf[0]
						proto := parser.buf[1]
						if ver > 1 {
							modeRaw = true
						} else {
							if proto < 1 || proto > 3 {
								modeRaw = true
							} else {
								if ver == 0 {
									reqNeed = 2
									enableXray = proto == 3
								} else {
									if len(parser.buf) >= 3 {
										paddingEnabled := parser.buf[2] != 0
										if !paddingEnabled {
											reqNeed = 3
											enableXray = proto == 3
										} else if len(parser.buf) >= 5 {
											paddingLen := int(binary.BigEndian.Uint16(parser.buf[3:5]))
											reqNeed = 5 + paddingLen
											enableXray = proto == 3
										}
									}
								}
							}
						}
					}
				}
				if modeRaw {
					rawBuf.Write(parser.buf)
					parser.buf = nil
					holdSince = time.Time{}
					continue
				}
				if reqNeed > 0 && len(parser.buf) >= reqNeed {
					req := parser.buf[:reqNeed]
					b := c.getWriteBuf(len(req))
					copy(b, req)
					c.scheduler.enqueue(muxFrameMeta{}, b, true, c.seqMgr.NextSendSequence())
					parser.buf = parser.buf[reqNeed:]
					reqNeed = 0
					detectDone = true
					holdSince = time.Time{}
					if !enableXray && len(parser.buf) > 0 {
						modeRaw = true
						rawBuf.Write(parser.buf)
						parser.buf = nil
					}
				}
			}
			if enableXray && detectDone && !modeRaw {
				for {
					meta, frame, ok := parser.nextXrayFrame()
					if !ok {
						break
					}
					b := c.getWriteBuf(len(frame))
					copy(b, frame)
					c.scheduler.enqueue(meta, b, true, c.seqMgr.NextSendSequence())
				}
				if len(parser.buf) > 2+xrayMuxMaxMetadata+2+65535 {
					modeRaw = true
					rawBuf.Write(parser.buf)
					parser.buf = nil
					holdSince = time.Time{}
				}
			}

		case <-ticker.C:
			if modeRaw {
				flushRaw()
			} else if !detectDone && len(parser.buf) > 0 && !holdSince.IsZero() && time.Since(holdSince) >= 50*time.Millisecond {
				modeRaw = true
				rawBuf.Write(parser.buf)
				parser.buf = nil
				holdSince = time.Time{}
				flushRaw()
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
			c.fail(fmt.Errorf("read message: %w", err))
			return
		}

		c.bytesRecv.Add(uint64(len(payload)))

		// 处理消息
		if err := c.handleIncomingMessage(chunk, payload); err != nil {
			c.fail(err)
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
	if err := c.sendVideoSequenceHeader(); err != nil {
		c.fail(err)
		return
	}
	if err := c.sendAudioSequenceHeader(); err != nil {
		c.fail(err)
		return
	}

	frameCount := 0
	lastIdleSend := time.Now()
	lastSend := time.Time{}
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			hasPending := c.scheduler.hasPending()
			if !hasPending && time.Since(lastIdleSend) < time.Second {
				continue
			}
			if !hasPending {
				lastIdleSend = time.Now()
			}
			lastSend = time.Now()
			frameCount++
			// 每N帧发送一次音频标签
			audioEvery := c.cfg.FrameRate / 2
			if audioEvery < 1 {
				audioEvery = 1
			}
			sendAudio := frameCount%audioEvery == 0

			// 生成视频帧
			if err := c.sendVideoFrame(); err != nil {
				c.fail(err)
				return
			}

			if sendAudio {
				if err := c.sendAudioFrame(); err != nil {
					c.fail(err)
					return
				}
			}
		case <-c.schedNotify:
			if time.Since(lastSend) < frameInterval/4 {
				continue
			}
			if !c.scheduler.hasPending() {
				continue
			}
			lastSend = time.Now()
			frameCount++
			audioEvery := c.cfg.FrameRate / 2
			if audioEvery < 1 {
				audioEvery = 1
			}
			sendAudio := frameCount%audioEvery == 0
			if err := c.sendVideoFrame(); err != nil {
				c.fail(err)
				return
			}
			if sendAudio {
				if err := c.sendAudioFrame(); err != nil {
					c.fail(err)
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

// sendVideoFrame 发送单个视频帧
func (c *RTMPConn) sendVideoFrame() error {
	maxPerFrame := c.cfg.MaxSEIPerFrame
	if maxPerFrame <= 0 {
		maxPerFrame = 1
	}
	proxyDataList := make([][]byte, 0, maxPerFrame)
	maxFragSize := c.frag.MaxFragSize()
	connLabel := fmt.Sprintf("%d", c.connID)
	batchBytes := 0
	for len(proxyDataList) < maxPerFrame {
		frag, prio, enq, first := c.scheduler.nextFragment(c, maxFragSize)
		if len(frag) == 0 {
			break
		}
		batchBytes += len(frag)
		if first {
			d := time.Since(enq)
			switch prio {
			case prioP0:
				metrics.ObserveQueueWait(connLabel, "p0", d)
				if len(frag) < smallPacketThreshold {
					metrics.ObserveSmallPacketLatency(connLabel, d)
				}
			case prioP1:
				metrics.ObserveQueueWait(connLabel, "p1", d)
			default:
				metrics.ObserveQueueWait(connLabel, "p2", d)
			}
		}
		proxyDataList = append(proxyDataList, frag)
	}
	metrics.ObserveFlushBatchBytes(connLabel, batchBytes)

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

	msgLen := 5
	for _, nalu := range nalus {
		msgLen += 4 + len(nalu)
	}
	var header [5]byte
	header[0] = (frameType&0x0F)<<4 | (container.VideoCodecAVC & 0x0F)
	header[1] = container.AVCNALU

	prefixBuf := c.getVideoPayloadBuf()
	if cap(prefixBuf) < 4*len(nalus) {
		prefixBuf = make([]byte, 4*len(nalus))
	} else {
		prefixBuf = prefixBuf[:4*len(nalus)]
	}
	parts := make([][]byte, 0, 1+len(nalus)*2)
	parts = append(parts, header[:])
	for i, nalu := range nalus {
		p := prefixBuf[i*4 : (i+1)*4]
		binary.BigEndian.PutUint32(p, uint32(len(nalu)))
		parts = append(parts, p, nalu)
	}
	csID := uint32(4) // 视频块流
	if err := c.writeRTMPMessageParts(csID, container.TagTypeVideo, 1, ts, msgLen, parts...); err != nil {
		c.putVideoPayloadBuf(prefixBuf)
		return err
	}
	c.putVideoPayloadBuf(prefixBuf)

	c.framesSent.Add(1)
	c.bytesSent.Add(uint64(msgLen))
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
	return c.WriteRTMPMessage(csID, container.TagTypeAudio, 1, ts, payload)
}

// sendVideoSequenceHeader 发送视频序列头
func (c *RTMPConn) sendVideoSequenceHeader() error {
	nalus := c.gopSim.BuildSequenceHeader()
	ts := uint32(0)

	msgLen := 5
	for _, nalu := range nalus {
		msgLen += 4 + len(nalu)
	}
	var header [5]byte
	header[0] = (container.FrameTypeKeyFrame&0x0F)<<4 | (container.VideoCodecAVC & 0x0F)
	header[1] = container.AVCSequenceHeader

	prefixBuf := c.getVideoPayloadBuf()
	if cap(prefixBuf) < 4*len(nalus) {
		prefixBuf = make([]byte, 4*len(nalus))
	} else {
		prefixBuf = prefixBuf[:4*len(nalus)]
	}
	parts := make([][]byte, 0, 1+len(nalus)*2)
	parts = append(parts, header[:])
	for i, nalu := range nalus {
		p := prefixBuf[i*4 : (i+1)*4]
		binary.BigEndian.PutUint32(p, uint32(len(nalu)))
		parts = append(parts, p, nalu)
	}
	csID := uint32(4)
	if err := c.writeRTMPMessageParts(csID, container.TagTypeVideo, 1, ts, msgLen, parts...); err != nil {
		c.putVideoPayloadBuf(prefixBuf)
		return err
	}
	c.putVideoPayloadBuf(prefixBuf)
	return nil
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
	return c.WriteRTMPMessage(csID, container.TagTypeAudio, 1, ts, payload)
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
	return c.WriteRTMPMessage(msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload)
}

func (c *RTMPConn) handleServerCreateStream(cmd *command.Command) error {
	c.streamID++
	resp := command.BuildCreateStreamResult(cmd.TransactionID, c.streamID)
	msg, _ := resp.ToMessage(3)
	return c.WriteRTMPMessage(msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload)
}

func (c *RTMPConn) handleServerPublish(cmd *command.Command) error {
	// 发送onStatus响应
	status := command.BuildPublishStatus(c.streamID, true)
	msg, _ := status.ToMessage(3)
	if err := c.WriteRTMPMessage(msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload); err != nil {
		return err
	}

	// 发送StreamBegin
	return c.ctrlMsgs.SendStreamBegin(c, uint32(c.streamID))
}

func (c *RTMPConn) handleServerPlay(cmd *command.Command) error {
	// 发送onStatus响应
	status := command.BuildPlayStatus(c.streamID)
	msg, _ := status.ToMessage(3)
	if err := c.WriteRTMPMessage(msg.ChunkStreamID, msg.Type, msg.StreamID, msg.Timestamp, msg.Payload); err != nil {
		return err
	}

	// 发送StreamBegin
	return c.ctrlMsgs.SendStreamBegin(c, uint32(c.streamID))
}

func (c *RTMPConn) handleServerClose(cmd *command.Command) error {
	c.Close()
	return nil
}
