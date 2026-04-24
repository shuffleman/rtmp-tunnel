// protocol/chunk.go - RTMP块格式解析与组装
package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// RTMP消息类型常量
const (
	MsgTypeSetChunkSize     = 1
	MsgTypeAbortMessage     = 2
	MsgTypeAck              = 3
	MsgTypeUserControl      = 4
	MsgTypeWindowAckSize    = 5
	MsgTypeSetPeerBandwidth = 6
	MsgTypeAudio            = 8
	MsgTypeVideo            = 9
	MsgTypeDataAMF3         = 15
	MsgTypeSharedObjectAMF3 = 16
	MsgTypeCommandAMF3      = 17
	MsgTypeDataAMF0         = 18
	MsgTypeSharedObjectAMF0 = 19
	MsgTypeCommandAMF0      = 20
	MsgTypeAggregate        = 22
)

// 用户控制事件类型
const (
	UserCtrlStreamBegin      = 0
	UserCtrlStreamEOF        = 1
	UserCtrlStreamDry        = 2
	UserCtrlSetBufferLength  = 3
	UserCtrlStreamIsRecorded = 4
	UserCtrlPingRequest      = 6
	UserCtrlPingResponse     = 7
)

// BasicHeader 基本头 [fmt(2) + csid(3~22)]
type BasicHeader struct {
	Format   uint8  // 0-3
	StreamID uint32 // chunk stream id
}

// MessageHeader 消息头
type MessageHeader struct {
	Timestamp       uint32 // 绝对时间戳或增量
	MessageLength   uint32 // 消息长度
	MessageTypeID   uint8  // 消息类型
	MessageStreamID uint32 // 消息流ID (小端序)
}

// Chunk 完整的RTMP块
type Chunk struct {
	BasicHeader
	MessageHeader
	ExtendedTimestamp uint32
	Data              []byte // 当前块的载荷
}

// ChunkCodec 块的编解码器
type ChunkCodec struct {
	inMu      sync.Mutex
	outMu     sync.Mutex
	chunkSize atomic.Int32 // 当前块大小(从SetChunkSize协商)
	maxMessageSize atomic.Int32
	InStreams    map[uint32]*InChunkStream
	OutStreams   map[uint32]*OutChunkStream
	lastInHeaders  map[uint32]*MessageHeader // 用于头部压缩
	lastOutHeaders map[uint32]*MessageHeader
}

// InChunkStream 输入块流状态
type InChunkStream struct {
	StreamID        uint32
	ReceivedBytes   int      // 已接收字节
	ExpectedLength  int      // 消息总长度
	CurrentMessage  []byte   // 正在组装的消息
	CurrentHeader   *MessageHeader
	IsFirstChunk    bool
}

// OutChunkStream 输出块流状态
type OutChunkStream struct {
	StreamID     uint32
	LastHeader   *MessageHeader
	SequenceNum  uint32
}

// NewChunkCodec 创建块编解码器
func NewChunkCodec() *ChunkCodec {
	cc := &ChunkCodec{
		InStreams:      make(map[uint32]*InChunkStream),
		OutStreams:     make(map[uint32]*OutChunkStream),
		lastInHeaders:  make(map[uint32]*MessageHeader),
		lastOutHeaders: make(map[uint32]*MessageHeader),
	}
	cc.chunkSize.Store(128)
	cc.maxMessageSize.Store(4 * 1024 * 1024)
	return cc
}

// SetChunkSize 更新块大小
func (cc *ChunkCodec) SetChunkSize(size int) {
	if size > 0 && size <= 65536 {
		cc.chunkSize.Store(int32(size))
	}
}

func (cc *ChunkCodec) SetMaxMessageSize(size int) {
	if size > 0 {
		cc.maxMessageSize.Store(int32(size))
	}
}

// ReadChunk 从reader解析一个完整的块
func (cc *ChunkCodec) ReadChunk(r io.Reader) (*Chunk, error) {
	cc.inMu.Lock()
	defer cc.inMu.Unlock()
	return cc.readChunk(r)
}

func (cc *ChunkCodec) readChunk(r io.Reader) (*Chunk, error) {
	// 1. 读取基本头
	bh, err := cc.readBasicHeader(r)
	if err != nil {
		return nil, fmt.Errorf("read basic header: %w", err)
	}

	// 2. 读取消息头
	mh, extTs, err := cc.readMessageHeader(r, bh.Format, bh.StreamID)
	if err != nil {
		return nil, fmt.Errorf("read message header: %w", err)
	}

	// 3. 计算当前块的数据大小
	cs := cc.InStreams[bh.StreamID]
	if cs == nil {
		cs = &InChunkStream{StreamID: bh.StreamID, IsFirstChunk: true}
		cc.InStreams[bh.StreamID] = cs
	}

	var chunkDataLen int
	if cs.IsFirstChunk || cs.CurrentMessage == nil {
		// 新消息
		if maxSize := int(cc.maxMessageSize.Load()); maxSize > 0 && int(mh.MessageLength) > maxSize {
			return nil, fmt.Errorf("message too large: %d", mh.MessageLength)
		}
		cs.ExpectedLength = int(mh.MessageLength)
		capacity := min(int(mh.MessageLength), 4096)
		cs.CurrentMessage = make([]byte, 0, capacity)
		cs.CurrentHeader = mh
		cs.ReceivedBytes = 0
		cs.IsFirstChunk = false
		chunkDataLen = min(int(mh.MessageLength), cc.getChunkSize())
	} else {
		// 消息续传(类型2或3)
		remaining := cs.ExpectedLength - cs.ReceivedBytes
		chunkDataLen = min(remaining, cc.getChunkSize())
	}

	// 读取扩展时间戳
	if mh.Timestamp == 0xFFFFFF {
		if err := binary.Read(r, binary.BigEndian, &extTs); err != nil {
			return nil, fmt.Errorf("read extended timestamp: %w", err)
		}
	}

	// 4. 读取块数据
	data := make([]byte, chunkDataLen)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("read chunk data: %w", err)
	}

	cs.CurrentMessage = append(cs.CurrentMessage, data...)
	cs.ReceivedBytes += chunkDataLen

	// 更新最后接收的头(用于头部压缩)
	cc.lastInHeaders[bh.StreamID] = cs.CurrentHeader

	chunk := &Chunk{
		BasicHeader:       *bh,
		MessageHeader:     *cs.CurrentHeader,
		ExtendedTimestamp: extTs,
		Data:              data,
	}

	// 检查消息是否完整
	if cs.ReceivedBytes >= cs.ExpectedLength {
		cs.IsFirstChunk = true // 重置为下一条消息
	}

	return chunk, nil
}

// ReadCompleteMessage 读取完整的消息(可能跨多个块)
func (cc *ChunkCodec) ReadCompleteMessage(r io.Reader) (*Chunk, []byte, error) {
	cc.inMu.Lock()
	defer cc.inMu.Unlock()
	var fullMsg []byte
	var firstChunk *Chunk

	for {
		chunk, err := cc.readChunk(r)
		if err != nil {
			return nil, nil, err
		}
		if firstChunk == nil {
			firstChunk = chunk
		}

		cs := cc.InStreams[chunk.StreamID]
		if cs == nil {
			return nil, nil, fmt.Errorf("unknown chunk stream: %d", chunk.StreamID)
		}

		fullMsg = cs.CurrentMessage

		// 消息完整
		if cs.ReceivedBytes >= cs.ExpectedLength && len(fullMsg) >= cs.ExpectedLength {
			msgCopy := make([]byte, cs.ExpectedLength)
			copy(msgCopy, fullMsg[:cs.ExpectedLength])
			// 重置流状态
			cs.CurrentMessage = nil
			cs.ReceivedBytes = 0
			return firstChunk, msgCopy, nil
		}
	}
}

// WriteChunk 写入一个块到writer
func (cc *ChunkCodec) WriteChunk(w io.Writer, csID uint32, msgType uint8, streamID uint32, timestamp uint32, data []byte) error {
	cc.outMu.Lock()
	defer cc.outMu.Unlock()
	msgLen := len(data)
	if msgLen == 0 {
		return nil
	}

	// 确定消息头格式
	format := uint8(0) // 首次使用类型0完整头
	lastHeader := cc.lastOutHeaders[csID]
	if lastHeader != nil {
		if lastHeader.MessageLength == uint32(msgLen) && lastHeader.MessageTypeID == msgType &&
			lastHeader.MessageStreamID == streamID {
			format = 2 // 只发时间戳增量
		} else {
			format = 1 // 省略消息流ID
		}
	}

	offset := 0
	isFirst := true

	for offset < msgLen {
		chunkLen := min(msgLen-offset, cc.getChunkSize())
		chunkData := data[offset : offset+chunkLen]

		if isFirst {
			// 写基本头(格式0/1/2)
			if err := cc.writeBasicHeader(w, format, csID); err != nil {
				return err
			}
			if err := cc.writeMessageHeader(w, format, csID, timestamp, uint32(msgLen), msgType, streamID); err != nil {
				return err
			}
			isFirst = false
		} else {
			// 续传块使用格式3(无消息头)
			if err := cc.writeBasicHeader(w, 3, csID); err != nil {
				return err
			}
		}

		// 写块数据
		if _, err := w.Write(chunkData); err != nil {
			return fmt.Errorf("write chunk payload: %w", err)
		}

		offset += chunkLen
	}

	// 更新最后输出的头
	cc.lastOutHeaders[csID] = &MessageHeader{
		Timestamp:       timestamp,
		MessageLength:   uint32(msgLen),
		MessageTypeID:   msgType,
		MessageStreamID: streamID,
	}

	return nil
}

func (cc *ChunkCodec) getChunkSize() int {
	v := cc.chunkSize.Load()
	if v <= 0 {
		return 128
	}
	return int(v)
}

// readBasicHeader 读取基本头
func (cc *ChunkCodec) readBasicHeader(r io.Reader) (*BasicHeader, error) {
	var firstByte [1]byte
	if _, err := io.ReadFull(r, firstByte[:]); err != nil {
		return nil, err
	}

	format := (firstByte[0] >> 6) & 0x03
	csid := uint32(firstByte[0] & 0x3F)

	switch csid {
	case 0: // 2字节形式
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		csid = uint32(b[0]) + 64
	case 1: // 3字节形式
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return nil, err
		}
		csid = uint32(b[0]) + uint32(b[1])*256 + 64
	}

	return &BasicHeader{Format: format, StreamID: csid}, nil
}

// readMessageHeader 读取消息头
func (cc *ChunkCodec) readMessageHeader(r io.Reader, format uint8, csID uint32) (*MessageHeader, uint32, error) {
	var extTs uint32
	lastHeader := cc.lastInHeaders[csID]

	switch format {
	case 0: // 11字节完整头 [timestamp(3) + length(3) + type(1) + streamID(4)]
		var buf [11]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return nil, 0, err
		}
		ts := readUint24(buf[0:3])
		mh := &MessageHeader{
			Timestamp:       ts,
			MessageLength:   readUint24(buf[3:6]),
			MessageTypeID:   buf[6],
			MessageStreamID: binary.LittleEndian.Uint32(buf[7:11]),
		}
		if ts == 0xFFFFFF {
			binary.Read(r, binary.BigEndian, &extTs)
			mh.Timestamp = extTs
		}
		return mh, extTs, nil

	case 1: // 7字节头(省略消息流ID) [timestampDelta(3) + length(3) + type(1)]
		var buf [7]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return nil, 0, err
		}
		if lastHeader == nil {
			return nil, 0, fmt.Errorf("type 1 header with no previous header for csid %d", csID)
		}
		tsDelta := readUint24(buf[0:3])
		mh := &MessageHeader{
			Timestamp:       lastHeader.Timestamp + tsDelta,
			MessageLength:   readUint24(buf[3:6]),
			MessageTypeID:   buf[6],
			MessageStreamID: lastHeader.MessageStreamID,
		}
		if tsDelta == 0xFFFFFF {
			binary.Read(r, binary.BigEndian, &extTs)
			mh.Timestamp = lastHeader.Timestamp + extTs
		}
		return mh, extTs, nil

	case 2: // 3字节头(仅时间戳增量) [timestampDelta(3)]
		var buf [3]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return nil, 0, err
		}
		if lastHeader == nil {
			return nil, 0, fmt.Errorf("type 2 header with no previous header for csid %d", csID)
		}
		tsDelta := readUint24(buf[0:3])
		mh := &MessageHeader{
			Timestamp:       lastHeader.Timestamp + tsDelta,
			MessageLength:   lastHeader.MessageLength,
			MessageTypeID:   lastHeader.MessageTypeID,
			MessageStreamID: lastHeader.MessageStreamID,
		}
		if tsDelta == 0xFFFFFF {
			binary.Read(r, binary.BigEndian, &extTs)
			mh.Timestamp = lastHeader.Timestamp + extTs
		}
		return mh, extTs, nil

	case 3: // 0字节头(使用上一个头的所有字段)
		if lastHeader == nil {
			return nil, 0, fmt.Errorf("type 3 header with no previous header for csid %d", csID)
		}
		return &MessageHeader{
			Timestamp:       lastHeader.Timestamp,
			MessageLength:   lastHeader.MessageLength,
			MessageTypeID:   lastHeader.MessageTypeID,
			MessageStreamID: lastHeader.MessageStreamID,
		}, 0, nil

	default:
		return nil, 0, fmt.Errorf("unknown chunk format: %d", format)
	}
}

// writeBasicHeader 写入基本头
func (cc *ChunkCodec) writeBasicHeader(w io.Writer, format uint8, csID uint32) error {
	var buf [3]byte
	n := 1

	csidVal := csID
	if csID < 2 {
		csidVal = 2 // 最小合法csid
	}

	if csidVal < 64 {
		buf[0] = (format << 6) | byte(csidVal&0x3F)
	} else if csidVal < 320 {
		buf[0] = (format << 6) | 0x00
		buf[1] = byte(csidVal - 64)
		n = 2
	} else {
		buf[0] = (format << 6) | 0x01
		csidVal -= 64
		buf[1] = byte(csidVal & 0xFF)
		buf[2] = byte(csidVal >> 8)
		n = 3
	}

	_, err := w.Write(buf[:n])
	return err
}

// writeMessageHeader 写入消息头
func (cc *ChunkCodec) writeMessageHeader(w io.Writer, format uint8, csID uint32, timestamp, msgLen uint32, msgType uint8, streamID uint32) error {
	switch format {
	case 0: // 完整11字节
		var buf [11]byte
		if timestamp >= 0xFFFFFF {
			writeUint24(buf[0:3], 0xFFFFFF)
		} else {
			writeUint24(buf[0:3], timestamp)
		}
		writeUint24(buf[3:6], msgLen)
		buf[6] = msgType
		binary.LittleEndian.PutUint32(buf[7:11], streamID)
		_, err := w.Write(buf[:])
		if err == nil && timestamp >= 0xFFFFFF {
			err = binary.Write(w, binary.BigEndian, timestamp)
		}
		return err

	case 1: // 7字节
		var buf [7]byte
		lastHeader := cc.lastOutHeaders[csID]
		tsDelta := timestamp
		if lastHeader != nil {
			tsDelta = timestamp - lastHeader.Timestamp
		}
		if tsDelta >= 0xFFFFFF {
			writeUint24(buf[0:3], 0xFFFFFF)
		} else {
			writeUint24(buf[0:3], tsDelta)
		}
		writeUint24(buf[3:6], msgLen)
		buf[6] = msgType
		_, err := w.Write(buf[:])
		return err

	case 2: // 3字节
		var buf [3]byte
		lastHeader := cc.lastOutHeaders[csID]
		tsDelta := timestamp
		if lastHeader != nil {
			tsDelta = timestamp - lastHeader.Timestamp
		}
		if tsDelta >= 0xFFFFFF {
			writeUint24(buf[0:3], 0xFFFFFF)
		} else {
			writeUint24(buf[0:3], tsDelta)
		}
		_, err := w.Write(buf[:])
		return err

	case 3: // 无头
		return nil
	}
	return nil
}

// 辅助函数
func readUint24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func writeUint24(b []byte, v uint32) {
	b[0] = byte(v >> 16)
	b[1] = byte(v >> 8)
	b[2] = byte(v)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
