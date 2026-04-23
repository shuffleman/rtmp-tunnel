// command/commands.go - RTMP命令消息构造与解析
package command

import (
	"bytes"
	"fmt"
	"io"

	"github.com/shuffleman/rtmp-tunnel/protocol"
)

// CommandName 标准RTMP命令名称
const (
	CmdConnect       = "connect"
	CmdCall          = "call"
	CmdClose         = "close"
	CmdCreateStream  = "createStream"
	CmdDeleteStream  = "deleteStream"
	CmdPublish       = "publish"
	CmdPlay          = "play"
	CmdPause         = "pause"
	CmdSeek          = "seek"
	CmdReceiveAudio  = "receiveAudio"
	CmdReceiveVideo  = "receiveVideo"
	CmdReleaseStream = "releaseStream"
	CmdFCPublish     = "FCPublish"
	CmdFCSubscribe   = "FCSubscribe"
	Cmd_onBWDone     = "_onbwdone"
	Cmd_onFCSubscribe = "onFCSubscribe"
	Cmd_onFCPublish   = "onFCPublish"
	CmdOnStatus      = "onStatus"
	CmdResult        = "_result"
	CmdError         = "_error"
	CmdOnMetaData    = "onMetaData"
	CmdSetDataFrame  = "@setDataFrame"
)

// TransactionID 事务ID分配器
type TransactionID struct {
	current float64
}

// NewTransactionID 创建事务ID分配器
func NewTransactionID() *TransactionID {
	return &TransactionID{current: 0}
}

// Next 获取下一个事务ID
func (tid *TransactionID) Next() float64 {
	tid.current++
	return tid.current
}

// Command RTMP命令消息
type Command struct {
	Name          string
	TransactionID float64
	Objects       []AMF0Value // 命令对象(命令对象 + 可选参数)
}

// Encode 编码命令为字节流
func (cmd *Command) Encode() ([]byte, error) {
	var buf bytes.Buffer
	enc := NewAMF0Encoder(&buf)

	// 命令名
	if err := enc.Encode(AMF0String(cmd.Name)); err != nil {
		return nil, fmt.Errorf("encode command name: %w", err)
	}
	// 事务ID
	if err := enc.Encode(AMF0Number(cmd.TransactionID)); err != nil {
		return nil, fmt.Errorf("encode transaction id: %w", err)
	}
	// 对象
	for _, obj := range cmd.Objects {
		if err := enc.Encode(obj); err != nil {
			return nil, fmt.Errorf("encode command object: %w", err)
		}
	}

	return buf.Bytes(), nil
}

// DecodeCommand 从payload解码命令
func DecodeCommand(payload []byte) (*Command, error) {
	r := bytes.NewReader(payload)
	dec := NewAMF0Decoder(r)

	// 命令名
	nameVal, err := dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("decode command name: %w", err)
	}
	name := String(nameVal)

	// 事务ID
	tidVal, err := dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("decode transaction id: %w", err)
	}
	tid := Float64(tidVal)

	// 剩余对象为命令参数
	var objects []AMF0Value
	for r.Len() > 0 {
		obj, err := dec.Decode()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decode command object: %w", err)
		}
		objects = append(objects, obj)
	}

	return &Command{
		Name:          name,
		TransactionID: tid,
		Objects:       objects,
	}, nil
}

// ToMessage 将命令转换为RTMP消息
func (cmd *Command) ToMessage(chunkStreamID uint32) (*protocol.Message, error) {
	payload, err := cmd.Encode()
	if err != nil {
		return nil, err
	}

	return &protocol.Message{
		Type:          protocol.MsgTypeCommandAMF0,
		StreamID:      0,
		Timestamp:     0,
		Payload:       payload,
		ChunkStreamID: chunkStreamID,
	}, nil
}

// === 预定义的命令构造器 ===

// BuildConnect 构建connect命令
func BuildConnect(tid float64, appName string, tcURL string) *Command {
	cmdObj := AMF0Object(map[string]AMF0Value{
		"app":            AMF0String(appName),
		"type":           AMF0String("nonprivate"),
		"flashVer":       AMF0String("FMLE/3.0 (compatible; FMSc/1.0)"),
		"tcUrl":          AMF0String(tcURL),
		"fpad":           AMF0Bool(false),
		"capabilities":   AMF0Number(239),
		"audioCodecs":    AMF0Number(3191),
		"videoCodecs":    AMF0Number(252),
		"videoFunction":  AMF0Number(1),
		"pageUrl":        AMF0Null(),
		"objectEncoding": AMF0Number(0), // AMF0
	})

	return &Command{
		Name:          CmdConnect,
		TransactionID: tid,
		Objects:       []AMF0Value{cmdObj},
	}
}

// BuildCreateStream 构建createStream命令
func BuildCreateStream(tid float64) *Command {
	return &Command{
		Name:          CmdCreateStream,
		TransactionID: tid,
		Objects:       []AMF0Value{AMF0Null()},
	}
}

// BuildPublish 构建publish命令
func BuildPublish(tid float64, streamName string, streamType string) *Command {
	if streamType == "" {
		streamType = "live" // live/record/append
	}
	return &Command{
		Name:          CmdPublish,
		TransactionID: tid,
		Objects: []AMF0Value{
			AMF0Null(),
			AMF0String(streamName),
			AMF0String(streamType),
		},
	}
}

// BuildPlay 构建play命令
func BuildPlay(tid float64, streamName string) *Command {
	return &Command{
		Name:          CmdPlay,
		TransactionID: tid,
		Objects: []AMF0Value{
			AMF0Null(),
			AMF0String(streamName),
		},
	}
}

// BuildResult 构建_result响应
func BuildResult(tid float64, properties AMF0Value, info AMF0Value) *Command {
	objs := []AMF0Value{properties}
	if info != nil {
		objs = append(objs, info)
	}
	return &Command{
		Name:          CmdResult,
		TransactionID: tid,
		Objects:       objs,
	}
}

// BuildConnectResult 构建connect的_result响应
func BuildConnectResult(tid float64) *Command {
	props := AMF0Object(map[string]AMF0Value{
		"fmsVer":       AMF0String("FMS/3,5,7,7009"),
		"capabilities": AMF0Number(31),
		"mode":         AMF0Number(1),
	})
	info := AMF0Object(map[string]AMF0Value{
		"level":           AMF0String("status"),
		"code":            AMF0String("NetConnection.Connect.Success"),
		"description":     AMF0String("Connection succeeded."),
		"objectEncoding":  AMF0Number(0),
		"data": AMF0ECMAArrayValue{Properties: map[string]AMF0Value{
			"version": AMF0String("3,5,7,7009"),
		}},
	})

	return BuildResult(tid, props, info)
}

// BuildCreateStreamResult 构建createStream的_result响应
func BuildCreateStreamResult(tid float64, streamID float64) *Command {
	return BuildResult(tid, AMF0Null(), AMF0Number(streamID))
}

// BuildOnStatus 构建onStatus通知
func BuildOnStatus(streamID float64, level, code, description string) *Command {
	info := AMF0Object(map[string]AMF0Value{
		"level":       AMF0String(level),
		"code":        AMF0String(code),
		"description": AMF0String(description),
		"details":     AMF0String(""),
		"clientid":    AMF0Number(streamID),
	})

	return &Command{
		Name:          CmdOnStatus,
		TransactionID: 0,
		Objects:       []AMF0Value{AMF0Null(), info},
	}
}

// BuildPublishStatus 构建publish的onStatus响应
func BuildPublishStatus(streamID float64, success bool) *Command {
	if success {
		return BuildOnStatus(streamID, "status", "NetStream.Publish.Start", "Start publishing")
	}
	return BuildOnStatus(streamID, "error", "NetStream.Publish.BadName", "Stream already exists")
}

// BuildPlayStatus 构建play的onStatus响应
func BuildPlayStatus(streamID float64) *Command {
	return BuildOnStatus(streamID, "status", "NetStream.Play.Start", "Start playing")
}

// BuildReleaseStream 构建releaseStream命令(Flash Media Encoder兼容)
func BuildReleaseStream(tid float64, streamName string) *Command {
	return &Command{
		Name:          CmdReleaseStream,
		TransactionID: tid,
		Objects: []AMF0Value{
			AMF0Null(),
			AMF0String(streamName),
		},
	}
}

// BuildFCPublish 构建FCPublish命令
func BuildFCPublish(tid float64, streamName string) *Command {
	return &Command{
		Name:          CmdFCPublish,
		TransactionID: tid,
		Objects: []AMF0Value{
			AMF0Null(),
			AMF0String(streamName),
		},
	}
}

// BuildFCSubscribe 构建FCSubscribe命令
func BuildFCSubscribe(tid float64, streamName string) *Command {
	return &Command{
		Name:          CmdFCSubscribe,
		TransactionID: tid,
		Objects: []AMF0Value{
			AMF0Null(),
			AMF0String(streamName),
		},
	}
}

// BuildPause 构建pause命令
func BuildPause(tid float64, pause bool, position float64) *Command {
	return &Command{
		Name:          CmdPause,
		TransactionID: tid,
		Objects: []AMF0Value{
			AMF0Null(),
			AMF0Bool(pause),
			AMF0Number(position),
		},
	}
}

// BuildCloseStream 构建closeStream命令
func BuildCloseStream(tid float64) *Command {
	return &Command{
		Name:          CmdClose,
		TransactionID: tid,
		Objects:       []AMF0Value{AMF0Null()},
	}
}

// ParseOnStatus 解析onStatus消息中的关键字段
func ParseOnStatus(cmd *Command) (level, code, description string) {
	if len(cmd.Objects) < 2 {
		return "", "", ""
	}

	info, ok := cmd.Objects[1].(AMF0ObjectValue)
	if !ok {
		return "", "", ""
	}

	if v, ok := info.Properties["level"].(AMF0StringValue); ok {
		level = v.Value
	}
	if v, ok := info.Properties["code"].(AMF0StringValue); ok {
		code = v.Value
	}
	if v, ok := info.Properties["description"].(AMF0StringValue); ok {
		description = v.Value
	}
	return
}

// ParseConnectCommand 解析connect命令中的参数
func ParseConnectCommand(cmd *Command) map[string]interface{} {
	result := make(map[string]interface{})
	if len(cmd.Objects) < 1 {
		return result
	}

	obj, ok := cmd.Objects[0].(AMF0ObjectValue)
	if !ok {
		// 尝试ECMA数组
		ecma, ok := cmd.Objects[0].(AMF0ECMAArrayValue)
		if ok {
			for k, v := range ecma.Properties {
				switch val := v.(type) {
				case AMF0StringValue:
					result[k] = val.Value
				case AMF0NumberValue:
					result[k] = val.Value
				case AMF0BooleanValue:
					result[k] = val.Value
				}
			}
		}
		return result
	}

	for k, v := range obj.Properties {
		switch val := v.(type) {
		case AMF0StringValue:
			result[k] = val.Value
		case AMF0NumberValue:
			result[k] = val.Value
		case AMF0BooleanValue:
			result[k] = val.Value
		}
	}
	return result
}

// GetAppName 从connect参数中提取app名称
func GetAppName(params map[string]interface{}) string {
	if v, ok := params["app"].(string); ok {
		return v
	}
	return ""
}

// IsPublishStream 检查流名称是否为发布流
func IsPublishStream(name string, publishName string) bool {
	return name == publishName
}

// IsPlaybackStream 检查流名称是否为播放流
func IsPlaybackStream(name string, playbackName string) bool {
	return name == playbackName
}
