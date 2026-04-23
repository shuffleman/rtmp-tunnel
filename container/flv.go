// container/flv.go - FLV容器封装与解封装
package container

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// FLV常量
const (
	FLVHeaderSize        = 9
	FLVTagHeaderSize     = 11
	PreviousTagSizeField = 4

	// 标签类型
	TagTypeAudio  = 8
	TagTypeVideo  = 9
	TagTypeScript = 18

	// 视频编码标识
	VideoCodecAVC = 7 // H.264/AVC

	// 音频编码标识
	AudioCodecAAC = 10 // AAC

	// AVC包类型
	AVCSequenceHeader = 0 // SPS/PPS NALU
	AVCNALU           = 1 // 普通NALU
	AVCEndOfSequence  = 2 // 序列结束

	// AAC包类型
	AACSequenceHeader = 0 // AudioSpecificConfig
	AACRaw            = 1 // 原始AAC帧

	// 帧类型
	FrameTypeKeyFrame    = 1 // 关键帧(IDR)
	FrameTypeInterFrame  = 2 // 非关键帧(P帧)
	FrameTypeDisposable  = 3 // 可丢弃的非关键帧(H.263 only)
	FrameTypeGenerated   = 4 // 关键帧生成的帧
	FrameTypeVideoInfo   = 5 // 视频信息/命令帧
)

// FLVHeader FLV文件头
type FLVHeader struct {
	Signature  [3]byte // "FLV"
	Version    uint8
	Flags      uint8 // 第0bit=音频, 第2bit=视频
	HeaderSize uint32
}

// TagHeader FLV标签头
type TagHeader struct {
	Type       uint8  // 标签类型
	DataSize   uint32 // 数据大小(3字节)
	Timestamp  uint32 // 时间戳(3字节 + 1字节扩展)
	StreamID   uint32 // 流ID(3字节，总是0)
}

// Tag 完整FLV标签
type Tag struct {
	Header TagHeader
	Data   []byte // 标签数据(不含头)
}

// VideoTagData 视频标签数据结构
type VideoTagData struct {
	FrameType       uint8  // 4 bits
	CodecID         uint8  // 4 bits
	AVCPacketType   uint8  // 1 byte
	CompositionTime int32  // 3 bytes (有符号)
	NALUs           [][]byte // NAL单元列表
}

// AudioTagData 音频标签数据结构
type AudioTagData struct {
	SoundFormat   uint8 // 4 bits
	SoundRate     uint8 // 2 bits
	SoundSize     uint8 // 1 bit
	SoundType     uint8 // 1 bit
	AACPacketType uint8 // 1 byte (仅AAC)
	Payload       []byte
}

// FLVContainer FLV容器
type FLVContainer struct {
	startTime time.Time
	baseTimestamp uint32
}

// NewFLVContainer 创建FLV容器
func NewFLVContainer() *FLVContainer {
	return &FLVContainer{
		startTime: time.Now(),
	}
}

// WriteHeader 写入FLV文件头
func (fc *FLVContainer) WriteHeader(w io.Writer, hasAudio, hasVideo bool) error {
	var flags uint8
	if hasAudio {
		flags |= 0x04
	}
	if hasVideo {
		flags |= 0x01
	}

	header := make([]byte, FLVHeaderSize+PreviousTagSizeField)
	copy(header[0:3], []byte("FLV"))
	header[3] = 1 // version
	header[4] = flags
	binary.BigEndian.PutUint32(header[5:9], uint32(FLVHeaderSize))
	// PreviousTagSize0 = 0
	binary.BigEndian.PutUint32(header[9:13], 0)

	_, err := w.Write(header)
	return err
}

// ReadHeader 读取FLV文件头
func (fc *FLVContainer) ReadHeader(r io.Reader) (*FLVHeader, error) {
	var buf [FLVHeaderSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, err
	}

	h := &FLVHeader{
		Signature:  [3]byte{buf[0], buf[1], buf[2]},
		Version:    buf[3],
		Flags:      buf[4],
		HeaderSize: binary.BigEndian.Uint32(buf[5:9]),
	}

	if string(h.Signature[:]) != "FLV" {
		return nil, fmt.Errorf("invalid FLV signature")
	}

	// 跳过PreviousTagSize0
	var skip [4]byte
	if _, err := io.ReadFull(r, skip[:]); err != nil {
		return nil, err
	}

	return h, nil
}

// WriteTag 写入一个FLV标签
func (fc *FLVContainer) WriteTag(w io.Writer, tag *Tag) error {
	// 写标签头
	header := make([]byte, FLVTagHeaderSize)
	header[0] = tag.Header.Type
	writeUint24(header[1:4], tag.Header.DataSize)
	writeUint24(header[4:7], tag.Header.Timestamp&0xFFFFFF)
	header[7] = byte((tag.Header.Timestamp >> 24) & 0xFF) // 时间戳扩展
	writeUint24(header[8:11], tag.Header.StreamID)

	if _, err := w.Write(header); err != nil {
		return err
	}

	// 写标签数据
	if _, err := w.Write(tag.Data); err != nil {
		return err
	}

	// 写PreviousTagSize
	pts := make([]byte, 4)
	binary.BigEndian.PutUint32(pts, uint32(FLVTagHeaderSize+len(tag.Data)))
	_, err := w.Write(pts)
	return err
}

// ReadTag 读取一个FLV标签
func (fc *FLVContainer) ReadTag(r io.Reader) (*Tag, error) {
	// 读标签头
	var headerBuf [FLVTagHeaderSize]byte
	if _, err := io.ReadFull(r, headerBuf[:]); err != nil {
		return nil, err
	}

	th := TagHeader{
		Type:      headerBuf[0],
		DataSize:  readUint24(headerBuf[1:4]),
		Timestamp: readUint24(headerBuf[4:7]),
		StreamID:  readUint24(headerBuf[8:11]),
	}
	// 时间戳扩展
	th.Timestamp |= uint32(headerBuf[7]) << 24

	// 读标签数据
	data := make([]byte, th.DataSize)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}

	// 跳过PreviousTagSize
	var skip [4]byte
	if _, err := io.ReadFull(r, skip[:]); err != nil {
		return nil, err
	}

	return &Tag{Header: th, Data: data}, nil
}

// BuildVideoTag 构建视频标签
func (fc *FLVContainer) BuildVideoTag(timestamp uint32, frameType, avcPacketType uint8, compositionTime int32, nalus [][]byte) (*Tag, error) {
	// 计算数据大小
	dataSize := 5 // 1(FrameType+CodecID) + 1(AVCPacketType) + 3(CompositionTime)
	for _, nalu := range nalus {
		dataSize += 4 + len(nalu) // NALU长度前缀 + NALU数据
	}

	data := make([]byte, dataSize)
	data[0] = (frameType&0x0F)<<4 | (VideoCodecAVC & 0x0F)
	data[1] = avcPacketType
	writeInt24(data[2:5], compositionTime)

	offset := 5
	for _, nalu := range nalus {
		binary.BigEndian.PutUint32(data[offset:offset+4], uint32(len(nalu)))
		copy(data[offset+4:], nalu)
		offset += 4 + len(nalu)
	}

	return &Tag{
		Header: TagHeader{
			Type:      TagTypeVideo,
			DataSize:  uint32(dataSize),
			Timestamp: timestamp,
			StreamID:  0,
		},
		Data: data,
	}, nil
}

// BuildAudioTag 构建音频标签
func (fc *FLVContainer) BuildAudioTag(timestamp uint32, aacPacketType uint8, payload []byte) (*Tag, error) {
	// SoundFormat(4) | SoundRate(2) | SoundSize(1) | SoundType(1) = 1 byte
	// AAC: 44kHz, 16bit, stereo = 0b1010_1101 = 0xAD
	soundFlags := byte((AudioCodecAAC << 4) | (3 << 2) | (1 << 1) | 1) // AAC, 44kHz, 16bit, stereo

	dataSize := 1 + 1 + len(payload)
	data := make([]byte, dataSize)
	data[0] = soundFlags
	data[1] = aacPacketType
	copy(data[2:], payload)

	return &Tag{
		Header: TagHeader{
			Type:      TagTypeAudio,
			DataSize:  uint32(dataSize),
			Timestamp: timestamp,
			StreamID:  0,
		},
		Data: data,
	}, nil
}

// BuildScriptTag 构建脚本标签(onMetaData)
func (fc *FLVContainer) BuildScriptTag(timestamp uint32, metadata map[string]interface{}) (*Tag, error) {
	// 简化的metadata编码 - 实际应使用AMF0编码
	// 这里用简化的键值对格式
	var buf []byte
	buf = append(buf, 0x02) // string type marker for name
	name := "onMetaData"
	buf = appendUint16(buf, uint16(len(name)))
	buf = append(buf, []byte(name)...)

	// ECMA array marker
	buf = append(buf, 0x08)
	buf = appendUint32(buf, uint32(len(metadata)))

	for k, v := range metadata {
		buf = appendUint16(buf, uint16(len(k)))
		buf = append(buf, []byte(k)...)
		switch val := v.(type) {
		case float64:
			buf = append(buf, 0x00)
			buf = appendFloat64(buf, val)
		case string:
			buf = append(buf, 0x02)
			buf = appendUint16(buf, uint16(len(val)))
			buf = append(buf, []byte(val)...)
		case bool:
			buf = append(buf, 0x01)
			if val {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		}
	}

	// Object end
	buf = appendUint16(buf, 0)
	buf = append(buf, 0x09)

	return &Tag{
		Header: TagHeader{
			Type:      TagTypeScript,
			DataSize:  uint32(len(buf)),
			Timestamp: timestamp,
			StreamID:  0,
		},
		Data: buf,
	}, nil
}

// ParseVideoTag 解析视频标签
func (fc *FLVContainer) ParseVideoTag(data []byte) (*VideoTagData, error) {
	if len(data) < 5 {
		return nil, fmt.Errorf("video tag data too short")
	}

	vd := &VideoTagData{
		FrameType:     (data[0] >> 4) & 0x0F,
		CodecID:       data[0] & 0x0F,
		AVCPacketType: data[1],
		CompositionTime: int32(readInt24(data[2:5])),
	}

	if vd.CodecID != VideoCodecAVC {
		return vd, nil // 非AVC编码
	}

	// 解析NALU
	if vd.AVCPacketType == AVCNALU && len(data) > 5 {
		offset := 5
		for offset+4 <= len(data) {
			naluLen := binary.BigEndian.Uint32(data[offset : offset+4])
			if offset+4+int(naluLen) > len(data) {
				break
			}
			vd.NALUs = append(vd.NALUs, data[offset+4:offset+4+int(naluLen)])
			offset += 4 + int(naluLen)
		}
	}

	return vd, nil
}

// TagToRTMPMessage 将FLV标签转换为RTMP消息payload
func TagToRTMPMessage(tag *Tag) []byte {
	return tag.Data
}

// RTMPMessageToTagPayload 将RTMP消息payload包装为标签数据
func RTMPMessageToTagPayload(msgType uint8, payload []byte) []byte {
	return payload
}

// 辅助函数
func writeUint24(b []byte, v uint32) {
	b[0] = byte(v >> 16)
	b[1] = byte(v >> 8)
	b[2] = byte(v)
}

func readUint24(b []byte) uint32 {
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func writeInt24(b []byte, v int32) {
	b[0] = byte((v >> 16) & 0xFF)
	b[1] = byte((v >> 8) & 0xFF)
	b[2] = byte(v & 0xFF)
}

func readInt24(b []byte) int32 {
	v := uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
	if v&0x800000 != 0 {
		v |= 0xFF000000
	}
	return int32(v)
}

func appendUint16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendUint32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func appendFloat64(b []byte, v float64) []byte {
	bits := make([]byte, 8)
	binary.BigEndian.PutUint64(bits, uint64(v))
	return append(b, bits...)
}
