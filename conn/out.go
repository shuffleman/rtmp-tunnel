package conn

import (
	"fmt"
	"time"

	"github.com/shuffleman/rtmp-tunnel/metrics"
	"github.com/shuffleman/rtmp-tunnel/protocol"
)

type rtmpOutOp struct {
	plan *protocol.ChunkWritePlan
	done chan error
	kind string
}

func (c *RTMPConn) startOutLoop() {
	c.outOnce.Do(func() {
		go c.outLoop()
	})
}

func (c *RTMPConn) WriteRTMPMessage(csID uint32, msgType uint8, streamID uint32, timestamp uint32, payload []byte) error {
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	c.startOutLoop()
	plan, err := c.chunkCodec.BuildWritePlanParts(csID, msgType, streamID, timestamp, len(payload), payload)
	if err != nil {
		return err
	}
	kind := "ctrl"
	if msgType == protocol.MsgTypeAudio || msgType == protocol.MsgTypeVideo {
		kind = "media"
	}
	op := &rtmpOutOp{plan: plan, done: make(chan error, 1), kind: kind}
	if kind == "media" {
		select {
		case c.outMediaCh <- op:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	} else {
		select {
		case c.outCtrlCh <- op:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	}
	select {
	case err := <-op.done:
		return err
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *RTMPConn) writeRTMPMessageParts(csID uint32, msgType uint8, streamID uint32, timestamp uint32, msgLen int, parts ...[]byte) error {
	if c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	c.startOutLoop()
	plan, err := c.chunkCodec.BuildWritePlanParts(csID, msgType, streamID, timestamp, msgLen, parts...)
	if err != nil {
		return err
	}
	kind := "ctrl"
	if msgType == protocol.MsgTypeAudio || msgType == protocol.MsgTypeVideo {
		kind = "media"
	}
	op := &rtmpOutOp{plan: plan, done: make(chan error, 1), kind: kind}
	if kind == "media" {
		select {
		case c.outMediaCh <- op:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	} else {
		select {
		case c.outCtrlCh <- op:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	}
	select {
	case err := <-op.done:
		return err
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}

func (c *RTMPConn) outLoop() {
	connLabel := fmt.Sprintf("%d", c.connID)
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		var op *rtmpOutOp
		select {
		case op = <-c.outCtrlCh:
		default:
			select {
			case op = <-c.outCtrlCh:
			case op = <-c.outMediaCh:
			case <-c.ctx.Done():
				return
			}
		}
		if op == nil || op.plan == nil {
			continue
		}
		start := time.Now()
		if c.cfg.NetworkWriteTimeout > 0 {
			_ = c.tcpConn.SetWriteDeadline(time.Now().Add(c.cfg.NetworkWriteTimeout))
		}
		err := op.plan.WriteTo(c.tcpConn)
		_ = c.tcpConn.SetWriteDeadline(time.Time{})
		d := time.Since(start)
		metrics.ObserveSocketWrite(connLabel, op.kind, d)
		if c.cfg.SlowWriteThreshold > 0 && d > c.cfg.SlowWriteThreshold {
			target := 0
			if c.cfg.MaxPendingProxyBytes > 0 {
				target = c.cfg.MaxPendingProxyBytes / 2
			}
			c.scheduler.shedBulk(target)
		}
		op.plan.Release()
		op.plan = nil
		select {
		case op.done <- err:
		default:
		}
		if err != nil {
			c.fail(err)
			return
		}
	}
}
