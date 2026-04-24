// crypto/fragment.go - 数据分片与重组
package crypto

import (
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Fragmenter 数据分片器
type Fragmenter struct {
	maxFragSize int // 最大分片大小(不含头)
}

// NewFragmenter 创建分片器
func NewFragmenter(maxFragSize int) *Fragmenter {
	if maxFragSize <= 0 {
		maxFragSize = 1400 // 默认小于MTU
	}
	return &Fragmenter{maxFragSize: maxFragSize}
}

// Fragment 将数据分片
// 返回: [(header+payload), ...]
func (f *Fragmenter) Fragment(seqNum uint32, data []byte) [][]byte {
	if len(data) <= f.maxFragSize {
		// 无需分片
		header := PacketFragmentHeader{
			SequenceNum: seqNum,
			FragmentIdx: 0,
			TotalFrags:  1,
			PayloadLen:  uint16(len(data)),
			Flags:       0,
		}
		frag := make([]byte, FragmentHeaderSize+len(data))
		header.EncodeTo(frag[:FragmentHeaderSize])
		copy(frag[FragmentHeaderSize:], data)
		return [][]byte{frag}
	}

	totalFrags := (len(data) + f.maxFragSize - 1) / f.maxFragSize
	var fragments [][]byte

	offset := 0
	for i := 0; i < totalFrags; i++ {
		end := offset + f.maxFragSize
		if end > len(data) {
			end = len(data)
		}
		payload := data[offset:end]

		header := PacketFragmentHeader{
			SequenceNum: seqNum,
			FragmentIdx: uint16(i),
			TotalFrags:  uint16(totalFrags),
			PayloadLen:  uint16(len(payload)),
			Flags:       0,
		}

		frag := make([]byte, FragmentHeaderSize+len(payload))
		header.EncodeTo(frag[:FragmentHeaderSize])
		copy(frag[FragmentHeaderSize:], payload)
		fragments = append(fragments, frag)
		offset = end
	}

	return fragments
}

// Reassembler 数据重组器
type Reassembler struct {
	mu         sync.Mutex
	packets    map[uint32]*pendingPacket
	timeout    time.Duration
	lastClean  time.Time
	maxPackets int
	maxBytes   int
	pendingBytes int
}

// pendingPacket 待重组的包
type pendingPacket struct {
	seqNum       uint32
	fragments    map[uint16][]byte // index -> payload
	totalFrags   uint16
	receivedFrags uint16
	totalLen     int
	firstSeen    time.Time
}

// NewReassembler 创建重组器
func NewReassembler(timeout time.Duration) *Reassembler {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Reassembler{
		packets:   make(map[uint32]*pendingPacket),
		timeout:   timeout,
		lastClean: time.Now(),
	}
}

func (r *Reassembler) SetLimits(maxPackets, maxBytes int) {
	r.mu.Lock()
	r.maxPackets = maxPackets
	r.maxBytes = maxBytes
	r.mu.Unlock()
}

// AddFragment 添加一个分片，如果完整则返回重组后的数据
func (r *Reassembler) AddFragment(frag []byte) ([]byte, error) {
	if len(frag) < FragmentHeaderSize {
		return nil, fmt.Errorf("fragment too short: %d", len(frag))
	}

	header, err := DecodeFragmentHeader(frag[:FragmentHeaderSize])
	if err != nil {
		return nil, err
	}

	if header.PayloadLen == 0 || int(header.PayloadLen) > len(frag)-FragmentHeaderSize {
		return nil, fmt.Errorf("invalid payload length: %d", header.PayloadLen)
	}

	payload := frag[FragmentHeaderSize : FragmentHeaderSize+int(header.PayloadLen)]

	r.mu.Lock()
	defer r.mu.Unlock()

	// 清理超时包
	r.cleanup()

	pp, exists := r.packets[header.SequenceNum]
	if !exists {
		if header.TotalFrags == 0 {
			return nil, fmt.Errorf("invalid total fragments: %d", header.TotalFrags)
		}
		pp = &pendingPacket{
			seqNum:      header.SequenceNum,
			fragments:   make(map[uint16][]byte),
			totalFrags:  header.TotalFrags,
			firstSeen:   time.Now(),
		}
		r.evictIfNeeded(0)
		r.packets[header.SequenceNum] = pp
	}

	// 检查总分片数是否一致
	if pp.totalFrags != header.TotalFrags {
		return nil, fmt.Errorf("fragment count mismatch: expected %d, got %d", pp.totalFrags, header.TotalFrags)
	}

	// 存储分片(去重)
	if _, exists := pp.fragments[header.FragmentIdx]; !exists {
		cp := make([]byte, len(payload))
		copy(cp, payload)
		r.evictIfNeeded(len(cp))
		pp.fragments[header.FragmentIdx] = cp
		pp.receivedFrags++
		pp.totalLen += len(cp)
		r.pendingBytes += len(cp)
	}

	// 检查是否完整
	if pp.receivedFrags >= pp.totalFrags {
		// 重组
		out := make([]byte, pp.totalLen)
		off := 0
		for i := uint16(0); i < pp.totalFrags; i++ {
			data, ok := pp.fragments[i]
			if !ok {
				r.mu.Unlock()
				return nil, fmt.Errorf("missing fragment %d", i)
			}
			copy(out[off:], data)
			off += len(data)
		}
		delete(r.packets, header.SequenceNum)
		r.pendingBytes -= pp.totalLen
		if r.pendingBytes < 0 {
			r.pendingBytes = 0
		}
		return out, nil
	}

	return nil, nil // 还未完整
}

func (r *Reassembler) evictIfNeeded(incomingBytes int) {
	for {
		if r.maxPackets > 0 && len(r.packets) >= r.maxPackets {
			r.evictOldest()
			continue
		}
		if r.maxBytes > 0 && r.pendingBytes+incomingBytes > r.maxBytes {
			r.evictOldest()
			continue
		}
		break
	}
}

func (r *Reassembler) evictOldest() {
	var (
		oldestSeq uint32
		oldest    *pendingPacket
	)
	for seq, pp := range r.packets {
		if oldest == nil || pp.firstSeen.Before(oldest.firstSeen) {
			oldest = pp
			oldestSeq = seq
		}
	}
	if oldest == nil {
		return
	}
	delete(r.packets, oldestSeq)
	r.pendingBytes -= oldest.totalLen
	if r.pendingBytes < 0 {
		r.pendingBytes = 0
	}
}

// cleanup 清理超时的待重组包
func (r *Reassembler) cleanup() {
	if time.Since(r.lastClean) < r.timeout/2 {
		return // 不需要频繁清理
	}
	now := time.Now()
	for seqNum, pp := range r.packets {
		if now.Sub(pp.firstSeen) > r.timeout {
			delete(r.packets, seqNum)
			r.pendingBytes -= pp.totalLen
		}
	}
	if r.pendingBytes < 0 {
		r.pendingBytes = 0
	}
	r.lastClean = now
}

// PendingCount 返回待重组的包数量
func (r *Reassembler) PendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.packets)
}

// PacketFragmentHeader 数据分片帧头
type PacketFragmentHeader struct {
	SequenceNum  uint32 // 包序列号
	FragmentIdx  uint16 // 分片索引
	TotalFrags   uint16 // 总分片数
	PayloadLen   uint16 // 本段有效载荷长度
	Flags        uint8  // 标志位(加密/压缩等)
}

// FragmentHeaderSize 分片头大小
const FragmentHeaderSize = 11 // 4+2+2+2+1

// Encode 编码分片头
func (pfh *PacketFragmentHeader) Encode() []byte {
	buf := make([]byte, FragmentHeaderSize)
	pfh.EncodeTo(buf)
	return buf
}

func (pfh *PacketFragmentHeader) EncodeTo(buf []byte) {
	binary.BigEndian.PutUint32(buf[0:4], pfh.SequenceNum)
	binary.BigEndian.PutUint16(buf[4:6], pfh.FragmentIdx)
	binary.BigEndian.PutUint16(buf[6:8], pfh.TotalFrags)
	binary.BigEndian.PutUint16(buf[8:10], pfh.PayloadLen)
	buf[10] = pfh.Flags
}

// DecodeFragmentHeader 解码分片头
func DecodeFragmentHeader(data []byte) (*PacketFragmentHeader, error) {
	if len(data) < FragmentHeaderSize {
		return nil, fmt.Errorf("fragment header too short: %d", len(data))
	}
	return &PacketFragmentHeader{
		SequenceNum: binary.BigEndian.Uint32(data[0:4]),
		FragmentIdx: binary.BigEndian.Uint16(data[4:6]),
		TotalFrags:  binary.BigEndian.Uint16(data[6:8]),
		PayloadLen:  binary.BigEndian.Uint16(data[8:10]),
		Flags:       data[10],
	}, nil
}

// SequenceManager 序列号管理器
type SequenceManager struct {
	mu        sync.Mutex
	sendSeq   uint32
	recvSeqs  map[uint32]bool // 已接收的序列号
	maxRecvSeq uint32
}

// NewSequenceManager 创建序列号管理器
func NewSequenceManager() *SequenceManager {
	return &SequenceManager{
		recvSeqs: make(map[uint32]bool),
	}
}

// NextSendSequence 获取下一个发送序列号
func (sm *SequenceManager) NextSendSequence() uint32 {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sendSeq++
	return sm.sendSeq
}

// MarkReceived 标记序列号已接收，返回是否重复
func (sm *SequenceManager) MarkReceived(seq uint32) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.recvSeqs[seq] {
		return true // 重复
	}
	sm.recvSeqs[seq] = true
	if seq > sm.maxRecvSeq {
		sm.maxRecvSeq = seq
	}

	// 清理旧序列号(保留最近的10000个)
	if len(sm.recvSeqs) > 20000 {
		sm.newCleanup()
	}

	return false
}

// 辅助方法用于清理旧序列号
func (sm *SequenceManager) newCleanup() {
	threshold := sm.maxRecvSeq - 10000
	for seq := range sm.recvSeqs {
		if seq < threshold {
			delete(sm.recvSeqs, seq)
		}
	}
}

// sortFragments 按分片索引排序
func sortFragments(frags []*PacketFragmentHeader) {
	sort.Slice(frags, func(i, j int) bool {
		return frags[i].FragmentIdx < frags[j].FragmentIdx
	})
}
