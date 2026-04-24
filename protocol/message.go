// protocol/message.go - RTMP消息结构与控制消息处理
package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Message RTMP完整消息
type Message struct {
	Type          uint8
	StreamID      uint32
	Timestamp     uint32
	Payload       []byte
	ChunkStreamID uint32
}

// ControlMessages 控制消息处理器
type ControlMessages struct {
	chunkCodec        *ChunkCodec
	peerChunkSize     int
	peerWindowSize    uint32
	peerBandwidth     uint32
	onChunkSizeChange func(int)
}

// NewControlMessages 创建控制消息处理器
func NewControlMessages(cc *ChunkCodec) *ControlMessages {
	return &ControlMessages{
		chunkCodec:     cc,
		peerChunkSize:  128,
		peerWindowSize: 2500000,
	}
}

// OnChunkSizeChange 设置块大小变更回调
func (cm *ControlMessages) OnChunkSizeChange(cb func(int)) {
	cm.onChunkSizeChange = cb
}

// SendSetChunkSize 发送设置块大小消息
func (cm *ControlMessages) SendSetChunkSize(w io.Writer, size int) error {
	if size < 1 || size > 0x7FFFFFFF {
		return fmt.Errorf("invalid chunk size: %d", size)
	}
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(size))
	return cm.chunkCodec.WriteChunk(w, 2, MsgTypeSetChunkSize, 0, 0, payload)
}

// SendWindowAckSize 发送窗口确认大小
func (cm *ControlMessages) SendWindowAckSize(w io.Writer, size uint32) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, size)
	return cm.chunkCodec.WriteChunk(w, 2, MsgTypeWindowAckSize, 0, 0, payload)
}

// SendSetPeerBandwidth 发送设置对等带宽
func (cm *ControlMessages) SendSetPeerBandwidth(w io.Writer, bandwidth uint32, limitType uint8) error {
	payload := make([]byte, 5)
	binary.BigEndian.PutUint32(payload, bandwidth)
	payload[4] = limitType
	return cm.chunkCodec.WriteChunk(w, 2, MsgTypeSetPeerBandwidth, 0, 0, payload)
}

// SendUserControl 发送用户控制消息
func (cm *ControlMessages) SendUserControl(w io.Writer, eventType uint16, data []byte) error {
	payload := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(payload, eventType)
	copy(payload[2:], data)
	return cm.chunkCodec.WriteChunk(w, 2, MsgTypeUserControl, 0, 0, payload)
}

// SendStreamBegin 发送流开始事件
func (cm *ControlMessages) SendStreamBegin(w io.Writer, streamID uint32) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, streamID)
	return cm.SendUserControl(w, UserCtrlStreamBegin, data)
}

// SendPingRequest 发送Ping请求
func (cm *ControlMessages) SendPingRequest(w io.Writer, timestamp uint32) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, timestamp)
	return cm.SendUserControl(w, UserCtrlPingRequest, data)
}

// SendPingResponse 发送Ping响应
func (cm *ControlMessages) SendPingResponse(w io.Writer, timestamp uint32) error {
	data := make([]byte, 4)
	binary.BigEndian.PutUint32(data, timestamp)
	return cm.SendUserControl(w, UserCtrlPingResponse, data)
}

// SendAck 发送字节确认
func (cm *ControlMessages) SendAck(w io.Writer, sequenceNumber uint32) error {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, sequenceNumber)
	return cm.chunkCodec.WriteChunk(w, 2, MsgTypeAck, 0, 0, payload)
}

// HandleControlMessage 处理接收到的控制消息
func (cm *ControlMessages) HandleControlMessage(msg *Message) error {
	switch msg.Type {
	case MsgTypeSetChunkSize:
		if len(msg.Payload) < 4 {
			return fmt.Errorf("invalid set chunk size payload")
		}
		size := int(binary.BigEndian.Uint32(msg.Payload[:4]))
		if size < 1 || size > 0x7FFFFFFF {
			return fmt.Errorf("invalid peer chunk size: %d", size)
		}
		cm.peerChunkSize = size
		if cm.onChunkSizeChange != nil {
			cm.onChunkSizeChange(size)
		}

	case MsgTypeWindowAckSize:
		if len(msg.Payload) >= 4 {
			cm.peerWindowSize = binary.BigEndian.Uint32(msg.Payload[:4])
		}

	case MsgTypeSetPeerBandwidth:
		if len(msg.Payload) >= 5 {
			cm.peerBandwidth = binary.BigEndian.Uint32(msg.Payload[:4])
		}

	case MsgTypeUserControl:
		if len(msg.Payload) < 2 {
			return fmt.Errorf("invalid user control payload")
		}
		eventType := binary.BigEndian.Uint16(msg.Payload[:2])
		data := msg.Payload[2:]
		return cm.handleUserControl(eventType, data)

	case MsgTypeAck:
		// 收到字节确认，可用于流量控制

	default:
		return fmt.Errorf("unknown control message type: %d", msg.Type)
	}
	return nil
}

// handleUserControl 处理用户控制事件
func (cm *ControlMessages) handleUserControl(eventType uint16, data []byte) error {
	switch eventType {
	case UserCtrlStreamBegin:
		if len(data) >= 4 {
			streamID := binary.BigEndian.Uint32(data[:4])
			_ = streamID
			// 流开始事件处理
		}

	case UserCtrlPingRequest:
		if len(data) >= 4 {
			ts := binary.BigEndian.Uint32(data[:4])
			_ = ts
			// 应响应PingResponse
		}

	case UserCtrlPingResponse:
		// Ping响应，可计算RTT

	case UserCtrlStreamEOF, UserCtrlStreamDry:
		// 流结束/干燥事件

	default:
		return fmt.Errorf("unknown user control event: %d", eventType)
	}
	return nil
}

// SendInitialControlMessages 发送初始控制消息(握手完成后)
func (cm *ControlMessages) SendInitialControlMessages(w io.Writer, chunkSize int) error {
	// 设置较大的块大小
	if err := cm.SendSetChunkSize(w, chunkSize); err != nil {
		return fmt.Errorf("set chunk size: %w", err)
	}
	cm.chunkCodec.SetChunkSize(chunkSize)

	// 设置窗口确认大小
	if err := cm.SendWindowAckSize(w, 5000000); err != nil {
		return fmt.Errorf("window ack size: %w", err)
	}

	// 设置对等带宽
	if err := cm.SendSetPeerBandwidth(w, 5000000, 2); err != nil {
		return fmt.Errorf("set peer bandwidth: %w", err)
	}

	return nil
}

// StartControlMessageLoop 启动控制消息维护循环(周期性ping/ack)
func (cm *ControlMessages) StartControlMessageLoop(w io.Writer, done <-chan struct{}, onError func(error)) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var bytesReceived uint32
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// 发送ping
			ts := uint32(time.Now().Unix())
			if err := cm.SendPingRequest(w, ts); err != nil {
				if onError != nil {
					onError(err)
				}
				return
			}

			// 发送ack
			bytesReceived += 100000 // 应由实际接收更新
			if err := cm.SendAck(w, bytesReceived); err != nil {
				if onError != nil {
					onError(err)
				}
				return
			}
		}
	}
}
