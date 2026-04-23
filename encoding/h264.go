// encoding/h264.go - H.264语法构造与NAL单元生成
package encoding

import (
	"crypto/rand"
	"fmt"
)

// NAL单元类型
const (
	NALUnspecified    = 0
	NALNonIDRSlice    = 1
	NALPartitionA     = 2
	NALPartitionB     = 3
	NALPartitionC     = 4
	NALIDRSlice       = 5
	NALSEI            = 6
	NALSPS            = 7
	NALPPS            = 8
	NALAUD            = 9
	NALEndOfSequence  = 10
	NALEndOfStream    = 11
	NALFillerData     = 12
	NALPrefix         = 14
	NALSubsetSPS      = 15
	NALReserved17     = 17
	NALReserved18     = 18
	NALNonIDRSliceAux = 19
	NALCodedSliceExt  = 20
)

// SEI Payload类型
const (
	SEIBufferingPeriod       = 0
	SEIPicTiming             = 1
	SEIPanScanRect           = 2
	SEIFillerPayload         = 3
	SEIUserDataRegistered    = 4
	SEIUserDataUnregistered  = 5
	SEIRecoveryPoint         = 6
	SEIDecRefPicMarkingRepet = 7
)

// H264Encoder H.264语法编码器
type H264Encoder struct {
	profileIDC        uint8
	constraintSet     uint8
	levelIDC          uint8
	seqParameterSetID uint8
	log2MaxFrameNum   uint8
	picOrderCntType   uint8
	maxNumRefFrames   uint8
	picWidthInMBS     uint16
	picHeightInMBS    uint16
	frameMBSOnlyFlag  uint8
}

// NewH264Encoder 创建H.264编码器
func NewH264Encoder(width, height int) *H264Encoder {
	// 计算宏块尺寸
	mbWidth := (width + 15) / 16
	mbHeight := (height + 15) / 16

	return &H264Encoder{
		profileIDC:        66, // Baseline profile (最通用兼容)
		constraintSet:     0xC0,
		levelIDC:          31, // Level 3.1 (支持720p@30fps)
		seqParameterSetID: 0,
		log2MaxFrameNum:   4,
		picOrderCntType:   2, // 简单POC类型
		maxNumRefFrames:   1,
		picWidthInMBS:     uint16(mbWidth),
		picHeightInMBS:    uint16(mbHeight),
		frameMBSOnlyFlag:  1,
	}
}

// BuildSPS 构建序列参数集(SPS) NAL单元
func (enc *H264Encoder) BuildSPS() []byte {
	// 使用简化的SPS构造
	// profile_idc(8) | constraint_set(8) | level_idc(8) | seq_parameter_set_id | ...
	rbsb := &bitWriter{}

	// profile_idc u(8)
	rbsb.WriteBits(uint64(enc.profileIDC), 8)
	// constraint_set0_flag u(1) = 1
	// constraint_set1_flag u(1) = 1
	// constraint_set2_flag u(1) = 0
	// constraint_set3_flag u(1) = 0
	// constraint_set4_flag u(1) = 0
	// constraint_set5_flag u(1) = 0
	// reserved_zero_2bits u(2) = 0
	rbsb.WriteBits(uint64(enc.constraintSet), 8)
	// level_idc u(8)
	rbsb.WriteBits(uint64(enc.levelIDC), 8)

	// seq_parameter_set_id ue(v) = 0
	rbsb.WriteUE(0)
	// log2_max_frame_num_minus4 ue(v)
	rbsb.WriteUE(uint64(enc.log2MaxFrameNum - 4))
	// pic_order_cnt_type ue(v)
	rbsb.WriteUE(uint64(enc.picOrderCntType))
	// log2_max_pic_order_cnt_lsb_minus4 ue(v) (only for poc_type=0)
	if enc.picOrderCntType == 0 {
		rbsb.WriteUE(0)
	}
	// max_num_ref_frames ue(v)
	rbsb.WriteUE(uint64(enc.maxNumRefFrames))
	// gaps_in_frame_num_value_allowed_flag u(1) = 0
	rbsb.WriteBits(0, 1)
	// pic_width_in_mbs_minus1 ue(v)
	rbsb.WriteUE(uint64(enc.picWidthInMBS - 1))
	// pic_height_in_map_units_minus1 ue(v)
	rbsb.WriteUE(uint64(enc.picHeightInMBS - 1))
	// frame_mbs_only_flag u(1) = 1
	rbsb.WriteBits(uint64(enc.frameMBSOnlyFlag), 1)
	// direct_8x8_inference_flag u(1) = 1
	rbsb.WriteBits(1, 1)
	// frame_cropping_flag u(1) = 0
	rbsb.WriteBits(0, 1)
	// vui_parameters_present_flag u(1) = 0
	rbsb.WriteBits(0, 1)
	// RBSP trailing bits
	rbsb.WriteBits(1, 1) // rbsp_stop_one_bit
	rbsb.ByteAlign()

	rbsp := rbsb.Bytes()

	// 添加NAL头 (nal_ref_idc=3, nal_unit_type=7)
	nalHeader := []byte{(3 << 5) | NALSPS}
	nalu := append(nalHeader, rbsp...)

	// 插入防止竞争字节
	return insertEmulationPrevention(nalu)
}

// BuildPPS 构建图像参数集(PPS) NAL单元
func (enc *H264Encoder) BuildPPS() []byte {
	rbsb := &bitWriter{}

	// pic_parameter_set_id ue(v) = 0
	rbsb.WriteUE(0)
	// seq_parameter_set_id ue(v) = 0
	rbsb.WriteUE(0)
	// entropy_coding_mode_flag u(1) = 0 (CAVLC)
	rbsb.WriteBits(0, 1)
	// bottom_field_pic_order_in_frame_present_flag u(1) = 0
	rbsb.WriteBits(0, 1)
	// num_slice_groups_minus1 ue(v) = 0
	rbsb.WriteUE(0)
	// num_ref_idx_l0_default_active_minus1 ue(v) = 0
	rbsb.WriteUE(0)
	// num_ref_idx_l1_default_active_minus1 ue(v) = 0
	rbsb.WriteUE(0)
	// weighted_pred_flag u(1) = 0
	rbsb.WriteBits(0, 1)
	// weighted_bipred_idc u(2) = 0
	rbsb.WriteBits(0, 2)
	// pic_init_qp_minus26 se(v) = 0
	rbsb.WriteSE(0)
	// pic_init_qs_minus26 se(v) = 0
	rbsb.WriteSE(0)
	// chroma_qp_index_offset se(v) = 0
	rbsb.WriteSE(0)
	// deblocking_filter_control_present_flag u(1) = 0
	rbsb.WriteBits(0, 1)
	// constrained_intra_pred_flag u(1) = 0
	rbsb.WriteBits(0, 1)
	// redundant_pic_cnt_present_flag u(1) = 0
	rbsb.WriteBits(0, 1)

	// RBSP trailing bits
	rbsb.WriteBits(1, 1)
	rbsb.ByteAlign()

	rbsp := rbsb.Bytes()

	// NAL头 (nal_ref_idc=3, nal_unit_type=8)
	nalHeader := []byte{(3 << 5) | NALPPS}
	nalu := append(nalHeader, rbsp...)

	return insertEmulationPrevention(nalu)
}

// BuildIDRSlice 构建IDR帧切片
func (enc *H264Encoder) BuildIDRSlice(mbCount int, payload []byte) []byte {
	rbsb := &bitWriter{}

	// first_mb_in_slice ue(v) = 0
	rbsb.WriteUE(0)
	// slice_type ue(v) = 7 (I slice, all MBs intra)
	rbsb.WriteUE(7)
	// pic_parameter_set_id ue(v) = 0
	rbsb.WriteUE(0)
	// frame_num u(log2_max_frame_num) = 0
	rbsb.WriteBits(0, uint8(enc.log2MaxFrameNum))
	// idr_pic_id ue(v)
	rbsb.WriteUE(1)

	// 写入宏块数据
	for i := 0; i < mbCount; i++ {
		// mb_skip_run ue(v) = 0 (no skip for I-slice)
		if i == 0 {
			rbsb.WriteUE(0)
		}
		// mb_type ue(v) = 25 (I_16x16_0_0_0)
		rbsb.WriteUE(25)
		// transform_size_8x8_flag u(1) = 0
		rbsb.WriteBits(0, 1)
		// intra_chroma_pred_mode ue(v) = 0 (DC)
		rbsb.WriteUE(0)
		// mb_qp_delta se(v) = 0
		rbsb.WriteSE(0)
		// 16x16 DC系数 (简化: 全零)
		for j := 0; j < 16; j++ {
			rbsb.WriteSE(0) // coeff_token 简化处理
		}
	}

	// RBSP trailing bits
	rbsb.WriteBits(1, 1)
	rbsb.ByteAlign()

	rbsp := rbsb.Bytes()

	// 嵌入payload(如果有)
	if len(payload) > 0 {
		rbsp = append(rbsp, payload...)
	}

	// NAL头 (nal_ref_idc=3, nal_unit_type=5)
	nalHeader := []byte{(3 << 5) | NALIDRSlice}
	nalu := append(nalHeader, rbsp...)

	return insertEmulationPrevention(nalu)
}

// BuildPFrameSlice 构建P帧切片(模拟非关键帧)
func (enc *H264Encoder) BuildPFrameSlice(mbCount int, payload []byte) []byte {
	rbsb := &bitWriter{}

	// first_mb_in_slice ue(v) = 0
	rbsb.WriteUE(0)
	// slice_type ue(v) = 5 (P slice)
	rbsb.WriteUE(5)
	// pic_parameter_set_id ue(v) = 0
	rbsb.WriteUE(0)
	// frame_num (递增)
	rbsb.WriteBits(1, uint8(enc.log2MaxFrameNum))

	// 写入宏块数据 - P帧宏块全部skip(节省比特)
	if mbCount > 0 {
		// mb_skip_run = mbCount (全部skip)
		rbsb.WriteUE(uint64(mbCount))
	}

	// RBSP trailing bits
	rbsb.WriteBits(1, 1)
	rbsb.ByteAlign()

	rbsp := rbsb.Bytes()

	if len(payload) > 0 {
		rbsp = append(rbsp, payload...)
	}

	// NAL头 (nal_ref_idc=2, nal_unit_type=1)
	nalHeader := []byte{(2 << 5) | NALNonIDRSlice}
	nalu := append(nalHeader, rbsp...)

	return insertEmulationPrevention(nalu)
}

// BuildSEIUserDataUnregistered 构建SEI用户数据未注册消息(用于嵌入代理数据)
func (enc *H264Encoder) BuildSEIUserDataUnregistered(uuid [16]byte, userData []byte) []byte {
	rbsb := &bitWriter{}

	// sei_message()
	// payload_type = 5 (user_data_unregistered)
	// payload_type 可能使用多个0xFF前缀
	payloadType := SEIUserDataUnregistered
	for payloadType >= 255 {
		rbsb.WriteBits(0xFF, 8)
		payloadType -= 255
	}
	rbsb.WriteBits(uint64(payloadType), 8)

	// payload_size
	payloadSize := 16 + len(userData) // UUID + userData
	for payloadSize >= 255 {
		rbsb.WriteBits(0xFF, 8)
		payloadSize -= 255
	}
	rbsb.WriteBits(uint64(payloadSize), 8)

	// UUID (16 bytes)
	for _, b := range uuid {
		rbsb.WriteBits(uint64(b), 8)
	}

	// 用户数据
	for _, b := range userData {
		rbsb.WriteBits(uint64(b), 8)
	}

	// RBSP trailing bits
	rbsb.WriteBits(1, 1)
	rbsb.ByteAlign()

	rbsp := rbsb.Bytes()

	// NAL头 (nal_ref_idc=0, nal_unit_type=6)
	nalHeader := []byte{(0 << 5) | NALSEI}
	nalu := append(nalHeader, rbsp...)

	return insertEmulationPrevention(nalu)
}

// BuildAUD 构建访问单元分隔符
func (enc *H264Encoder) BuildAUD(primaryPicType uint8) []byte {
	rbsb := &bitWriter{}
	rbsb.WriteBits(uint64(primaryPicType), 3)
	rbsb.WriteBits(1, 1) // rbsp_stop_one_bit
	rbsb.ByteAlign()

	rbsp := rbsb.Bytes()
	nalHeader := []byte{(0 << 5) | NALAUD}
	nalu := append(nalHeader, rbsp...)

	return insertEmulationPrevention(nalu)
}

// BuildEndOfSequence 构建序列结束NAL单元
func (enc *H264Encoder) BuildEndOfSequence() []byte {
	// 非常简单，只有NAL头
	nalu := []byte{(0 << 5) | NALEndOfSequence}
	return insertEmulationPrevention(nalu)
}

// ParseNALUType 解析NAL单元类型
func ParseNALUType(nalu []byte) uint8 {
	if len(nalu) < 1 {
		return NALUnspecified
	}
	return nalu[0] & 0x1F
}

// ParseNALRefIDC 解析NAL参考IDC
func ParseNALRefIDC(nalu []byte) uint8 {
	if len(nalu) < 1 {
		return 0
	}
	return (nalu[0] >> 5) & 0x03
}

// 防止竞争字节插入
func insertEmulationPrevention(nalu []byte) []byte {
	if len(nalu) < 3 {
		return nalu
	}

	result := make([]byte, 0, len(nalu)+len(nalu)/100)
	result = append(result, nalu[0], nalu[1])

	zeroCount := 0
	if nalu[0] == 0 {
		zeroCount++
	}
	if nalu[1] == 0 {
		zeroCount++
	} else {
		zeroCount = 0
	}

	for i := 2; i < len(nalu); i++ {
		if zeroCount == 2 && (nalu[i] == 0 || nalu[i] == 1 || nalu[i] == 2 || nalu[i] == 3) {
			result = append(result, 0x03) // 插入防止竞争字节
			zeroCount = 0                 // 重置:插入的03不是0
		}
		result = append(result, nalu[i])
		if nalu[i] == 0 {
			if zeroCount < 2 {
				zeroCount++
			}
			// zeroCount保持<=2，避免过度插入
		} else {
			zeroCount = 0
		}
	}

	return result
}

// RemoveEmulationPrevention 移除防止竞争字节
func RemoveEmulationPrevention(nalu []byte) []byte {
	if len(nalu) < 4 {
		return nalu
	}

	result := make([]byte, 0, len(nalu))
	zeroCount := 0

	for _, b := range nalu {
		if zeroCount == 2 && b == 0x03 {
			// 跳过0x03(防止竞争字节)
			zeroCount = 0
			continue
		}
		result = append(result, b)
		if b == 0 {
			if zeroCount < 2 {
				zeroCount++
			}
			// zeroCount保持在2(用于检测下一个03)
		} else {
			zeroCount = 0
		}
	}

	return result
}

// RandomUUID 生成随机UUID
func RandomUUID() [16]byte {
	var uuid [16]byte
	rand.Read(uuid[:])
	// 设置版本4位
	uuid[6] = (uuid[6] & 0x0F) | 0x40
	uuid[8] = (uuid[8] & 0x3F) | 0x80
	return uuid
}

// StandardUUID 返回标准的固定UUID(用于识别代理SEI)
func StandardUUID() [16]byte {
	// 使用固定UUID前缀标识这是代理数据SEI
	// 格式: 524D5450-5455-4E4E-454C-000000000000
	return [16]byte{
		0x52, 0x4D, 0x54, 0x50, // "RMTP"
		0x54, 0x55, 0x4E, 0x4E, // "TUNN"
		0x45, 0x4C, // "EL"
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
}

// IsProxySEI 检查NAL单元是否为代理数据SEI
func IsProxySEI(nalu []byte) bool {
	if ParseNALUType(nalu) != NALSEI {
		return false
	}
	rbsp := RemoveEmulationPrevention(nalu)
	// 跳过NAL头
	if len(rbsp) < 1+16 {
		return false
	}

	// 解析SEI payload
	offset := 1 // 跳过NAL头
	// 读取payload_type
	pt := 0
	for offset < len(rbsp) && rbsp[offset] == 0xFF {
		pt += 255
		offset++
	}
	if offset < len(rbsp) {
		pt += int(rbsp[offset])
		offset++
	}
	if pt != SEIUserDataUnregistered {
		return false
	}

	// 跳过payload_size
	for offset < len(rbsp) && rbsp[offset] == 0xFF {
		offset++
	}
	if offset < len(rbsp) {
		offset++ // payload_size最后一个字节
	}

	// 检查UUID
	if offset+16 > len(rbsp) {
		return false
	}

	stdUUID := StandardUUID()
	for i := 0; i < 16; i++ {
		if rbsp[offset+i] != stdUUID[i] {
			return false
		}
	}
	return true
}

// ExtractSEIPayload 从代理SEI NAL单元中提取payload
func ExtractSEIPayload(nalu []byte) ([]byte, error) {
	if !IsProxySEI(nalu) {
		return nil, fmt.Errorf("not a proxy SEI")
	}

	rbsp := RemoveEmulationPrevention(nalu)
	offset := 1 // 跳过NAL头

	// 跳过payload_type
	for offset < len(rbsp) && rbsp[offset] == 0xFF {
		offset++
	}
	offset++ // 最后一个payload_type字节

	// 跳过payload_size
	ps := 0
	for offset < len(rbsp) && rbsp[offset] == 0xFF {
		ps += 255
		offset++
	}
	if offset < len(rbsp) {
		ps += int(rbsp[offset])
		offset++
	}

	// 跳过UUID (16 bytes)
	offset += 16

	// 剩余为payload
	if offset >= len(rbsp) {
		return nil, nil
	}

	if ps < 16 {
		return nil, fmt.Errorf("invalid payload size: %d", ps)
	}
	end := offset + (ps - 16)
	if end > len(rbsp) {
		return nil, fmt.Errorf("payload overflow: %d > %d", end, len(rbsp))
	}

	payload := make([]byte, end-offset)
	copy(payload, rbsp[offset:end])
	return payload, nil
}

// bitWriter 位写入器
type bitWriter struct {
	buf   []byte
	bits  uint64
	nbits uint8
}

func (bw *bitWriter) WriteBits(v uint64, n uint8) {
	bw.bits <<= n
	bw.bits |= v & ((1 << n) - 1)
	bw.nbits += n

	for bw.nbits >= 8 {
		bw.nbits -= 8
		bw.buf = append(bw.buf, byte(bw.bits>>bw.nbits))
		bw.bits &= (1 << bw.nbits) - 1
	}
}

func (bw *bitWriter) WriteUE(v uint64) {
	// 指数哥伦布编码 (Unsigned Exp-Golomb)
	if v == 0 {
		bw.WriteBits(1, 1)
		return
	}
	v++
	bits := uint8(0)
	tmp := v
	for tmp > 0 {
		bits++
		tmp >>= 1
	}
	leadingZeros := bits - 1
	bw.WriteBits(0, leadingZeros)
	bw.WriteBits(v, bits)
}

func (bw *bitWriter) WriteSE(v int64) {
	// 有符号指数哥伦布编码
	var uv uint64
	if v > 0 {
		uv = uint64(2*v - 1)
	} else {
		uv = uint64(-2 * v)
	}
	bw.WriteUE(uv)
}

func (bw *bitWriter) ByteAlign() {
	if bw.nbits > 0 {
		bw.WriteBits(0, 8-bw.nbits)
	}
}

func (bw *bitWriter) Bytes() []byte {
	return bw.buf
}

// 辅助函数
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
