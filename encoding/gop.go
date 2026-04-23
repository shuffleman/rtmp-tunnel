// encoding/gop.go - GOP模拟器：按固定帧率与GOP长度生成帧
package encoding

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	mrand "math/rand"
	"sync"
	"time"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/container"
)

// GOPSimulator GOP模拟器，负责生成模拟视频帧
type GOPSimulator struct {
	cfg *config.Config

	// 帧序列状态
	frameNum  uint32 // 当前帧号
	gopNum    uint32 // 当前GOP号
	tsBase    uint32 // 基准时间戳(ms)
	startTime time.Time

	// 帧大小计算
	bytesPerFrame  int     // 每帧目标字节数
	idrFrameRatio  float64 // IDR帧大小比例
	pFrameMinRatio float64 // P帧最小大小比例

	// 加密嵌入
	encoder *H264Encoder
	seiUUID [16]byte
	mbCount int

	// 随机源
	rng       sync.Mutex
	prng      *mrand.Rand
	jitterBuf []int // 时间戳抖动缓冲区
}

// NewGOPSimulator 创建GOP模拟器
func NewGOPSimulator(cfg *config.Config) *GOPSimulator {
	// 计算每帧目标大小
	bytesPerFrame := cfg.TargetBitrate / 8 / cfg.FrameRate

	// 假设720p分辨率
	width, height := 1280, 720
	mbCount := (width / 16) * (height / 16) // 80 * 45 = 3600宏块

	gs := &GOPSimulator{
		cfg:            cfg,
		startTime:      time.Now(),
		bytesPerFrame:  bytesPerFrame,
		idrFrameRatio:  10.0, // IDR帧是平均帧大小的10倍
		pFrameMinRatio: 0.3,  // P帧至少为平均大小的30%
		encoder:        NewH264Encoder(width, height),
		seiUUID:        StandardUUID(),
		mbCount:        mbCount,
		jitterBuf:      make([]int, 0, cfg.FrameRate),
	}
	gs.prng = mrand.New(mrand.NewSource(int64(gs.seed64())))

	// 预生成时间戳抖动模式
	gs.generateJitterPattern()

	return gs
}

// SetResolution 设置分辨率
func (gs *GOPSimulator) SetResolution(width, height int) {
	gs.encoder = NewH264Encoder(width, height)
	gs.mbCount = (width / 16) * (height / 16)
}

// generateJitterPattern 生成时间戳抖动模式
func (gs *GOPSimulator) generateJitterPattern() {
	maxJitter := gs.cfg.TimestampJitterMax
	for i := 0; i < gs.cfg.FrameRate; i++ {
		// 使用正弦波+随机噪声生成自然抖动
		phase := 2 * math.Pi * float64(i) / float64(gs.cfg.FrameRate)
		jitter := int(math.Sin(phase) * float64(maxJitter) * 0.5)
		// 添加小随机扰动
		randBits := make([]byte, 1)
		crand.Read(randBits)
		jitter += int(randBits[0]%3) - 1
		gs.jitterBuf = append(gs.jitterBuf, jitter)
	}
}

// NextFrame 生成下一帧的所有NAL单元
// 返回: (帧类型, 时间戳, NAL单元列表)
func (gs *GOPSimulator) NextFrame(proxyData []byte) (frameType uint8, timestamp uint32, nalus [][]byte, err error) {
	if len(proxyData) == 0 {
		return gs.NextFrameMulti(nil)
	}
	return gs.NextFrameMulti([][]byte{proxyData})
}

func (gs *GOPSimulator) NextFrameMulti(proxyDataList [][]byte) (frameType uint8, timestamp uint32, nalus [][]byte, err error) {
	isIDR := gs.frameNum%uint32(gs.cfg.GOPSize) == 0

	// 计算时间戳
	ts := gs.calcTimestamp()

	var frameNALUs [][]byte

	// 1. AUD (每个帧开始)
	frameNALUs = append(frameNALUs, gs.encoder.BuildAUD(0))

	if isIDR {
		// IDR帧: SPS + PPS + IDR切片
		frameNALUs = append(frameNALUs, gs.encoder.BuildSPS())
		frameNALUs = append(frameNALUs, gs.encoder.BuildPPS())

		// 计算IDR帧目标大小
		targetSize := gs.calcIDRFrameSize()
		existingSize := gs.calcNALUsSize(frameNALUs)

		// 构建IDR切片，嵌入代理数据(如果有)
		var idrPayload []byte

		if len(proxyDataList) > 0 {
			for _, proxyData := range proxyDataList {
				if len(proxyData) == 0 {
					continue
				}
				seiNALU := gs.encoder.BuildSEIUserDataUnregistered(gs.seiUUID, proxyData)
				frameNALUs = append(frameNALUs, seiNALU)
				existingSize += len(seiNALU)
			}
		}

		// 填充至目标大小
		fillSize := targetSize - existingSize
		if fillSize < 0 {
			fillSize = 100 // 最小填充
		}
		idrPayload = gs.generateFillerData(fillSize)
		idrNALU := gs.encoder.BuildIDRSlice(gs.mbCount, idrPayload)
		frameNALUs = append(frameNALUs, idrNALU)

		return container.FrameTypeKeyFrame, ts, frameNALUs, nil
	}

	// P帧
	targetSize := gs.calcPFrameSize()
	existingSize := gs.calcNALUsSize(frameNALUs)

	// 如果还有代理数据，嵌入SEI
	if len(proxyDataList) > 0 {
		for _, proxyData := range proxyDataList {
			if len(proxyData) == 0 {
				continue
			}
			seiNALU := gs.encoder.BuildSEIUserDataUnregistered(gs.seiUUID, proxyData)
			frameNALUs = append(frameNALUs, seiNALU)
			existingSize += len(seiNALU)
		}
	}

	// 填充至目标大小
	fillSize := targetSize - existingSize
	if fillSize < 0 {
		fillSize = 50
	}
	pPayload := gs.generateFillerData(fillSize)
	pNALU := gs.encoder.BuildPFrameSlice(gs.mbCount, pPayload)
	frameNALUs = append(frameNALUs, pNALU)

	gs.frameNum++
	return container.FrameTypeInterFrame, ts, frameNALUs, nil
}

// calcTimestamp 计算当前帧的时间戳(ms)
func (gs *GOPSimulator) calcTimestamp() uint32 {
	elapsed := time.Since(gs.startTime).Milliseconds()
	frameInterval := 1000 / int64(gs.cfg.FrameRate)
	baseTS := int64(gs.frameNum) * frameInterval

	// 添加抖动
	jitterIdx := int(gs.frameNum) % len(gs.jitterBuf)
	jitter := int64(gs.jitterBuf[jitterIdx])

	ts := uint32(baseTS + jitter + elapsed)
	if ts < gs.tsBase {
		gs.tsBase = ts
	}
	return ts
}

// calcIDRFrameSize 计算IDR帧目标大小
func (gs *GOPSimulator) calcIDRFrameSize() int {
	// IDR帧大小 = 平均帧大小 * IDR比例 * 随机波动(0.9~1.1)
	base := float64(gs.bytesPerFrame) * gs.idrFrameRatio
	variation := 0.9 + float64(gs.randUint32()%21)/100.0 // 0.9 ~ 1.1
	size := int(base * variation)
	if gs.cfg.MaxFramePayloadSize > 0 && size > gs.cfg.MaxFramePayloadSize {
		size = gs.cfg.MaxFramePayloadSize
	}
	return size
}

// calcPFrameSize 计算P帧目标大小
func (gs *GOPSimulator) calcPFrameSize() int {
	// P帧大小在最小比例和平均大小之间波动
	minSize := float64(gs.bytesPerFrame) * gs.pFrameMinRatio
	maxSize := float64(gs.bytesPerFrame) * 1.5
	ratio := float64(gs.randUint32()) / float64(^uint32(0))

	size := minSize + (maxSize-minSize)*ratio
	out := int(size)
	if gs.cfg.MaxFramePayloadSize > 0 && out > gs.cfg.MaxFramePayloadSize {
		out = gs.cfg.MaxFramePayloadSize
	}
	return out
}

// calcNALUsSize 计算NAL单元总大小
func (gs *GOPSimulator) calcNALUsSize(nalus [][]byte) int {
	total := 0
	for _, nalu := range nalus {
		total += 4 + len(nalu) // 4字节长度前缀 + NALU
	}
	return total
}

// generateFillerData 生成填充数据(随机字节)
func (gs *GOPSimulator) generateFillerData(size int) []byte {
	if size <= 0 {
		return nil
	}
	data := make([]byte, size)
	fillWithPool(gs.randUint32(), data)
	return data
}

func (gs *GOPSimulator) randUint32() uint32 {
	gs.rng.Lock()
	v := gs.prng.Uint32()
	gs.rng.Unlock()
	return v
}

func (gs *GOPSimulator) seed64() uint64 {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])
}

var fillerOnce sync.Once
var fillerPool []byte

func fillWithPool(seed uint32, dst []byte) {
	fillerOnce.Do(func() {
		fillerPool = make([]byte, 1<<20)
		_, _ = crand.Read(fillerPool)
	})
	if len(dst) == 0 {
		return
	}
	off := int(seed) % len(fillerPool)
	n := 0
	for n < len(dst) {
		copied := copy(dst[n:], fillerPool[off:])
		n += copied
		off = 0
	}
}

// GenerateFillerFrame 生成纯填充帧(无代理数据)
func (gs *GOPSimulator) GenerateFillerFrame() (frameType uint8, timestamp uint32, nalus [][]byte, err error) {
	return gs.NextFrame(nil)
}

// Reset 重置帧计数器
func (gs *GOPSimulator) Reset() {
	gs.frameNum = 0
	gs.gopNum = 0
	gs.startTime = time.Now()
	gs.tsBase = 0
}

// FrameNum 返回当前帧号
func (gs *GOPSimulator) FrameNum() uint32 {
	return gs.frameNum
}

// GOPSize 返回GOP大小
func (gs *GOPSimulator) GOPSize() int {
	return gs.cfg.GOPSize
}

// FrameRate 返回帧率
func (gs *GOPSimulator) FrameRate() int {
	return gs.cfg.FrameRate
}

// BuildSequenceHeader 构建H.264序列头(SPS+PPS NALU)
func (gs *GOPSimulator) BuildSequenceHeader() [][]byte {
	return [][]byte{
		gs.encoder.BuildSPS(),
		gs.encoder.BuildPPS(),
	}
}

// GenerateAACSilentFrame 生成静音AAC帧
func GenerateAACSilentFrame() []byte {
	// AAC静音帧 (ADTS格式简化)
	// 这是一个预计算的合法AAC静音帧
	return []byte{
		0x21, 0x00, // ADTS头 (简化)
		0x00, 0x00, //
		// 实际AAC数据 - 静音
		0x00, 0x00, 0x00, 0x00,
	}
}

// BuildAACAudioSpecificConfig 构建AAC AudioSpecificConfig
func BuildAACAudioSpecificConfig() []byte {
	// AAC-LC, 44100Hz, stereo
	// audioObjectType = 2 (AAC-LC) u(5) = 00010
	// samplingFrequencyIndex = 4 (44100) u(4) = 0100
	// channelConfiguration = 2 (stereo) u(4) = 0010
	// = 0001 0010 0000 1000 = 0x12 0x08
	return []byte{0x12, 0x08}
}

// PacketFragmentHeader 数据分片帧头
type PacketFragmentHeader struct {
	SequenceNum uint32 // 包序列号
	FragmentIdx uint16 // 分片索引
	TotalFrags  uint16 // 总分片数
	PayloadLen  uint16 // 本段有效载荷长度
	Flags       uint8  // 标志位(加密/压缩等)
}

const FragmentHeaderSize = 11 // 4+2+2+2+1

// Encode 编码分片头
func (pfh *PacketFragmentHeader) Encode() []byte {
	buf := make([]byte, FragmentHeaderSize)
	binary.BigEndian.PutUint32(buf[0:4], pfh.SequenceNum)
	binary.BigEndian.PutUint16(buf[4:6], pfh.FragmentIdx)
	binary.BigEndian.PutUint16(buf[6:8], pfh.TotalFrags)
	binary.BigEndian.PutUint16(buf[8:10], pfh.PayloadLen)
	buf[10] = pfh.Flags
	return buf
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
