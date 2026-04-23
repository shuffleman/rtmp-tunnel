// command/amf0.go - AMF0编解码实现
package command

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// AMF0类型标记常量
const (
	TypeNumber        = 0x00
	TypeBoolean       = 0x01
	TypeString        = 0x02
	TypeObject        = 0x03
	TypeMovieClip     = 0x04
	TypeNull          = 0x05
	TypeUndefined     = 0x06
	TypeReference     = 0x07
	TypeECMAArray     = 0x08
	TypeObjectEnd     = 0x09
	TypeStrictArray   = 0x0A
	TypeDate          = 0x0B
	TypeLongString    = 0x0C
	TypeUnsupported   = 0x0D
	TypeRecordSet     = 0x0E
	TypeXMLDocument   = 0x0F
	TypeTypedObject   = 0x10
	TypeAVMPlusObject = 0x11
)

// AMF0Value AMF0值的通用接口
type AMF0Value interface {
	Type() byte
	Encode(w io.Writer) error
}

// AMF0NumberValue 数字值
type AMF0NumberValue struct{ Value float64 }

func (v AMF0NumberValue) Type() byte { return TypeNumber }
func (v AMF0NumberValue) Encode(w io.Writer) error {
	if err := writeByte(w, TypeNumber); err != nil { return err }
	return binary.Write(w, binary.BigEndian, v.Value)
}

// AMF0BooleanValue 布尔值
type AMF0BooleanValue struct{ Value bool }

func (v AMF0BooleanValue) Type() byte { return TypeBoolean }
func (v AMF0BooleanValue) Encode(w io.Writer) error {
	if err := writeByte(w, TypeBoolean); err != nil { return err }
	b := byte(0)
	if v.Value { b = 1 }
	return writeByte(w, b)
}

// AMF0StringValue 字符串值
type AMF0StringValue struct{ Value string }

func (v AMF0StringValue) Type() byte { return TypeString }
func (v AMF0StringValue) Encode(w io.Writer) error {
	if len(v.Value) > 65535 {
		if err := writeByte(w, TypeLongString); err != nil { return err }
		return writeLongString(w, v.Value)
	}
	if err := writeByte(w, TypeString); err != nil { return err }
	return writeString(w, v.Value)
}

// AMF0NullValue 空值
type AMF0NullValue struct{}

func (v AMF0NullValue) Type() byte { return TypeNull }
func (v AMF0NullValue) Encode(w io.Writer) error { return writeByte(w, TypeNull) }

// AMF0ObjectValue 对象值
type AMF0ObjectValue struct{ Properties map[string]AMF0Value }

func (v AMF0ObjectValue) Type() byte { return TypeObject }
func (v AMF0ObjectValue) Encode(w io.Writer) error {
	if err := writeByte(w, TypeObject); err != nil { return err }
	for k, val := range v.Properties {
		if err := writeString(w, k); err != nil { return err }
		if err := val.Encode(w); err != nil { return err }
	}
	return writeObjectEnd(w)
}

// AMF0ECMAArrayValue ECMA数组
type AMF0ECMAArrayValue struct{ Properties map[string]AMF0Value }

func (v AMF0ECMAArrayValue) Type() byte { return TypeECMAArray }
func (v AMF0ECMAArrayValue) Encode(w io.Writer) error {
	if err := writeByte(w, TypeECMAArray); err != nil { return err }
	if err := binary.Write(w, binary.BigEndian, uint32(len(v.Properties))); err != nil { return err }
	for k, val := range v.Properties {
		if err := writeString(w, k); err != nil { return err }
		if err := val.Encode(w); err != nil { return err }
	}
	return writeObjectEnd(w)
}

// AMF0StrictArrayValue 严格数组
type AMF0StrictArrayValue struct{ Elements []AMF0Value }

func (v AMF0StrictArrayValue) Type() byte { return TypeStrictArray }
func (v AMF0StrictArrayValue) Encode(w io.Writer) error {
	if err := writeByte(w, TypeStrictArray); err != nil { return err }
	if err := binary.Write(w, binary.BigEndian, uint32(len(v.Elements))); err != nil { return err }
	for _, el := range v.Elements {
		if err := el.Encode(w); err != nil { return err }
	}
	return nil
}

// AMF0DateValue 日期值
type AMF0DateValue struct{ Value time.Time }

func (v AMF0DateValue) Type() byte { return TypeDate }
func (v AMF0DateValue) Encode(w io.Writer) error {
	if err := writeByte(w, TypeDate); err != nil { return err }
	ms := float64(v.Value.UnixMilli())
	if err := binary.Write(w, binary.BigEndian, ms); err != nil { return err }
	return binary.Write(w, binary.BigEndian, int16(0))
}

// AMF0Encoder 编码器
type AMF0Encoder struct{ w io.Writer }

// NewAMF0Encoder 创建AMF0编码器
func NewAMF0Encoder(w io.Writer) *AMF0Encoder { return &AMF0Encoder{w: w} }

// Encode 编码单个值
func (enc *AMF0Encoder) Encode(v AMF0Value) error { return v.Encode(enc.w) }

// EncodeMultiple 编码多个值(用于命令参数)
func (enc *AMF0Encoder) EncodeMultiple(values ...AMF0Value) error {
	for _, v := range values {
		if err := enc.Encode(v); err != nil { return err }
	}
	return nil
}

// AMF0Decoder 解码器
type AMF0Decoder struct{ r io.Reader }

// NewAMF0Decoder 创建AMF0解码器
func NewAMF0Decoder(r io.Reader) *AMF0Decoder { return &AMF0Decoder{r: r} }

// Decode 解码单个值
func (dec *AMF0Decoder) Decode() (AMF0Value, error) {
	b, err := readByte(dec.r)
	if err != nil { return nil, err }

	switch b {
	case TypeNumber:
		var v float64
		if err := binary.Read(dec.r, binary.BigEndian, &v); err != nil { return nil, err }
		return AMF0NumberValue{Value: v}, nil

	case TypeBoolean:
		vb, err := readByte(dec.r)
		if err != nil { return nil, err }
		return AMF0BooleanValue{Value: vb != 0}, nil

	case TypeString:
		s, err := readString(dec.r)
		if err != nil { return nil, err }
		return AMF0StringValue{Value: s}, nil

	case TypeLongString:
		s, err := readLongString(dec.r)
		if err != nil { return nil, err }
		return AMF0StringValue{Value: s}, nil

	case TypeNull, TypeUndefined:
		return AMF0NullValue{}, nil

	case TypeObject:
		obj := AMF0ObjectValue{Properties: make(map[string]AMF0Value)}
		for {
			key, isEnd, err := readObjectKey(dec.r)
			if err != nil { return nil, err }
			if isEnd { break }
			val, err := dec.Decode()
			if err != nil { return nil, err }
			obj.Properties[key] = val
		}
		return obj, nil

	case TypeECMAArray:
		var count uint32
		if err := binary.Read(dec.r, binary.BigEndian, &count); err != nil { return nil, err }
		obj := AMF0ECMAArrayValue{Properties: make(map[string]AMF0Value)}
		for {
			key, isEnd, err := readObjectKey(dec.r)
			if err != nil { return nil, err }
			if isEnd { break }
			val, err := dec.Decode()
			if err != nil { return nil, err }
			obj.Properties[key] = val
		}
		return obj, nil

	case TypeStrictArray:
		var count uint32
		if err := binary.Read(dec.r, binary.BigEndian, &count); err != nil { return nil, err }
		arr := AMF0StrictArrayValue{Elements: make([]AMF0Value, 0, count)}
		for i := uint32(0); i < count; i++ {
			val, err := dec.Decode()
			if err != nil { return nil, err }
			arr.Elements = append(arr.Elements, val)
		}
		return arr, nil

	case TypeDate:
		var ms float64
		if err := binary.Read(dec.r, binary.BigEndian, &ms); err != nil { return nil, err }
		var tz int16
		if err := binary.Read(dec.r, binary.BigEndian, &tz); err != nil { return nil, err }
		_ = tz
		return AMF0DateValue{Value: time.UnixMilli(int64(ms))}, nil

	default:
		return nil, fmt.Errorf("unsupported AMF0 type: 0x%02X", b)
	}
}

// Convenience constructors
func AMF0Number(v float64) AMF0Value    { return AMF0NumberValue{Value: v} }
func AMF0String(v string) AMF0Value     { return AMF0StringValue{Value: v} }
func AMF0Bool(v bool) AMF0Value         { return AMF0BooleanValue{Value: v} }
func AMF0Null() AMF0Value               { return AMF0NullValue{} }
func AMF0Object(props map[string]AMF0Value) AMF0Value {
	return AMF0ObjectValue{Properties: props}
}

// Extractor helpers
func Float64(v AMF0Value) float64 {
	if nv, ok := v.(AMF0NumberValue); ok { return nv.Value }
	return 0
}

func String(v AMF0Value) string {
	if sv, ok := v.(AMF0StringValue); ok { return sv.Value }
	return ""
}

func Bool(v AMF0Value) bool {
	if bv, ok := v.(AMF0BooleanValue); ok { return bv.Value }
	return false
}

// 辅助函数
func writeByte(w io.Writer, b byte) error {
	_, err := w.Write([]byte{b})
	return err
}

func readByte(r io.Reader) (byte, error) {
	var b [1]byte
	_, err := io.ReadFull(r, b[:])
	return b[0], err
}

func writeString(w io.Writer, s string) error {
	b := []byte(s)
	if err := binary.Write(w, binary.BigEndian, uint16(len(b))); err != nil { return err }
	_, err := w.Write(b)
	return err
}

func readString(r io.Reader) (string, error) {
	var len16 uint16
	if err := binary.Read(r, binary.BigEndian, &len16); err != nil { return "", err }
	b := make([]byte, len16)
	if _, err := io.ReadFull(r, b); err != nil { return "", err }
	return string(b), nil
}

func writeLongString(w io.Writer, s string) error {
	b := []byte(s)
	if err := binary.Write(w, binary.BigEndian, uint32(len(b))); err != nil { return err }
	_, err := w.Write(b)
	return err
}

func readLongString(r io.Reader) (string, error) {
	var len32 uint32
	if err := binary.Read(r, binary.BigEndian, &len32); err != nil { return "", err }
	if len32 > 1024*1024 { return "", fmt.Errorf("long string too large: %d", len32) }
	b := make([]byte, len32)
	if _, err := io.ReadFull(r, b); err != nil { return "", err }
	return string(b), nil
}

func writeObjectEnd(w io.Writer) error {
	if err := binary.Write(w, binary.BigEndian, uint16(0)); err != nil { return err }
	return writeByte(w, TypeObjectEnd)
}

func readObjectKey(r io.Reader) (string, bool, error) {
	var len16 uint16
	if err := binary.Read(r, binary.BigEndian, &len16); err != nil { return "", false, err }
	if len16 == 0 {
		b, err := readByte(r)
		if err != nil { return "", false, err }
		if b == TypeObjectEnd { return "", true, nil }
		return "", false, fmt.Errorf("expected object end, got 0x%02X", b)
	}
	b := make([]byte, len16)
	if _, err := io.ReadFull(r, b); err != nil { return "", false, err }
	return string(b), false, nil
}
