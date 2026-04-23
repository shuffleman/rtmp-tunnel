package protocol

import (
	"bytes"
	"testing"
)

// TestChunkCodecBasicHeader 测试基本头编解码
func TestChunkCodecBasicHeader(t *testing.T) {
	cc := NewChunkCodec()
	tests := []struct {
		format uint8
		csid   uint32
	}{
		{0, 2},
		{1, 10},
		{2, 50},
		{3, 63},
		{0, 64},
		{1, 100},
		{2, 319},
		{3, 320},
		{0, 65599},
	}

	for _, tc := range tests {
		var buf bytes.Buffer
		err := cc.writeBasicHeader(&buf, tc.format, tc.csid)
		if err != nil {
			t.Fatalf("writeBasicHeader(format=%d, csid=%d): %v", tc.format, tc.csid, err)
		}

		r := bytes.NewReader(buf.Bytes())
		bh, err := cc.readBasicHeader(r)
		if err != nil {
			t.Fatalf("readBasicHeader(format=%d, csid=%d): %v", tc.format, tc.csid, err)
		}
		if bh.Format != tc.format {
			t.Errorf("format mismatch: got %d, want %d", bh.Format, tc.format)
		}
		if bh.StreamID != tc.csid {
			t.Errorf("csid mismatch: got %d, want %d", bh.StreamID, tc.csid)
		}
	}
}

// TestChunkCodecMessageHeader 测试消息头编解码
func TestChunkCodecMessageHeader(t *testing.T) {
	cc := NewChunkCodec()

	// 先写入一个类型0头，建立lastInHeaders
	var buf bytes.Buffer
	mh0 := &MessageHeader{
		Timestamp:       1000,
		MessageLength:   4096,
		MessageTypeID:   MsgTypeVideo,
		MessageStreamID: 1,
	}
	cc.lastInHeaders[4] = mh0

	err := cc.writeMessageHeader(&buf, 0, 4, 1000, 4096, MsgTypeVideo, 1)
	if err != nil {
		t.Fatalf("writeMessageHeader type0: %v", err)
	}

	// 读取类型0头
	r := bytes.NewReader(buf.Bytes())
	mh, extTs, err := cc.readMessageHeader(r, 0, 4)
	if err != nil {
		t.Fatalf("readMessageHeader type0: %v", err)
	}
	if mh.Timestamp != 1000 {
		t.Errorf("timestamp: got %d, want 1000", mh.Timestamp)
	}
	if mh.MessageLength != 4096 {
		t.Errorf("messageLength: got %d, want 4096", mh.MessageLength)
	}
	if mh.MessageTypeID != MsgTypeVideo {
		t.Errorf("messageTypeID: got %d, want %d", mh.MessageTypeID, MsgTypeVideo)
	}
	_ = extTs
}

// TestUint24 测试24位整数编解码
func TestUint24(t *testing.T) {
	tests := []uint32{0, 1, 255, 256, 65535, 65536, 0x7FFFFF, 0xFFFFFF}
	for _, v := range tests {
		var b [3]byte
		writeUint24(b[:], v)
		got := readUint24(b[:])
		if got != v {
			t.Errorf("uint24 roundtrip: got %d, want %d", got, v)
		}
	}
}
