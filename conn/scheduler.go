package conn

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/shuffleman/rtmp-tunnel/crypto"
)

type schedPriority uint8

const (
	prioP0 schedPriority = iota
	prioP1
	prioP2
)

const (
	smallPacketThreshold = 1024
	xrayMuxMaxMetadata   = 512
)

type muxFrameMeta struct {
	ok        bool
	sessionID uint16
	status    byte
	option    byte
	network   byte
	dstPort   uint16
	dataLen   int
	totalLen  int
}

type muxFrameParser struct {
	buf []byte
}

func (p *muxFrameParser) push(b []byte) {
	if len(b) == 0 {
		return
	}
	p.buf = append(p.buf, b...)
}

func (p *muxFrameParser) nextXrayFrame() (muxFrameMeta, []byte, bool, bool) {
	if len(p.buf) < 2 {
		return muxFrameMeta{}, nil, false, false
	}
	metaLen := int(binary.BigEndian.Uint16(p.buf[:2]))
	if metaLen < 4 || metaLen > xrayMuxMaxMetadata {
		return muxFrameMeta{}, nil, false, true
	}
	if len(p.buf) < 2+metaLen {
		return muxFrameMeta{}, nil, false, false
	}
	meta := p.buf[2 : 2+metaLen]
	m := muxFrameMeta{
		ok:        true,
		sessionID: binary.BigEndian.Uint16(meta[0:2]),
		status:    meta[2],
		option:    meta[3],
	}
	if m.status != 0x01 && m.status != 0x02 && m.status != 0x03 && m.status != 0x04 {
		return muxFrameMeta{}, nil, false, true
	}
	off := 4
	if m.status == 0x01 {
		if len(meta) < off+1+2 {
			return muxFrameMeta{}, nil, false, true
		}
		m.network = meta[off]
		if m.network != 0x01 && m.network != 0x02 {
			return muxFrameMeta{}, nil, false, true
		}
		off++
		m.dstPort = binary.BigEndian.Uint16(meta[off : off+2])
	}
	pos := 2 + metaLen
	if m.option&0x01 != 0 {
		if len(p.buf) < pos+2 {
			return muxFrameMeta{}, nil, false, false
		}
		dataLen := int(binary.BigEndian.Uint16(p.buf[pos : pos+2]))
		pos += 2
		if dataLen < 0 || dataLen > 65535 {
			return muxFrameMeta{}, nil, false, true
		}
		if len(p.buf) < pos+dataLen {
			return muxFrameMeta{}, nil, false, false
		}
		m.dataLen = dataLen
		pos += dataLen
	}
	m.totalLen = pos
	frame := p.buf[:pos]
	p.buf = p.buf[pos:]
	return m, frame, true, false
}

type schedItem struct {
	sessionID   uint16
	prio        schedPriority
	enqueued    time.Time
	plain       []byte
	plainLen    int
	plainPooled bool

	seq        uint32
	encrypted  []byte
	fragIdx    int
	totalFrags int
	firstSent  bool
}

type schedStream struct {
	id      uint16
	deficit int
	queues  [3][]*schedItem
	current *schedItem

	activeMask uint8
	items      int
}

type proxyScheduler struct {
	mu     sync.Mutex
	cond   *sync.Cond
	closed bool

	maxBytes int
	maxItems int

	streams map[uint16]*schedStream
	active  [3][]uint16
	rrPos   [3]int

	backlogBytes int
	backlogItems int

	notify  func()
	release func([]byte)
}

func newProxyScheduler(maxBytes, maxItems int) *proxyScheduler {
	s := &proxyScheduler{
		maxBytes: maxBytes,
		maxItems: maxItems,
		streams:  make(map[uint16]*schedStream),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *proxyScheduler) setHooks(notify func(), release func([]byte)) {
	s.mu.Lock()
	s.notify = notify
	s.release = release
	s.mu.Unlock()
}

func (s *proxyScheduler) close() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	rel := s.release
	s.release = nil
	s.notify = nil
	s.mu.Unlock()
	_ = rel
}

func (s *proxyScheduler) hasPending() bool {
	s.mu.Lock()
	ok := s.backlogItems > 0
	s.mu.Unlock()
	return ok
}

func classifyXrayFrame(meta muxFrameMeta) schedPriority {
	if meta.status == 0x01 {
		return prioP0
	}
	if meta.network == 0x02 && meta.dstPort == 53 {
		return prioP0
	}
	if meta.dataLen > 0 && meta.dataLen < smallPacketThreshold {
		return prioP0
	}
	if meta.dataLen > 0 && meta.dataLen <= 16*1024 {
		return prioP1
	}
	return prioP2
}

func (s *proxyScheduler) exceedsLocked(p schedPriority, nextBytes int) bool {
	maxBytes := s.maxBytes
	maxItems := s.maxItems
	if p == prioP0 {
		if maxBytes > 0 {
			maxBytes += 256 * 1024
		}
		if maxItems > 0 {
			maxItems += 256
		}
	}
	if maxBytes > 0 && s.backlogBytes+nextBytes > maxBytes {
		return true
	}
	if maxItems > 0 && s.backlogItems+1 > maxItems {
		return true
	}
	return false
}

func (s *proxyScheduler) ensureStreamLocked(id uint16) *schedStream {
	st := s.streams[id]
	if st != nil {
		return st
	}
	st = &schedStream{id: id}
	s.streams[id] = st
	return st
}

func (s *proxyScheduler) removeActiveLocked(p schedPriority, sid uint16) {
	ids := s.active[p]
	for i := range ids {
		if ids[i] != sid {
			continue
		}
		last := len(ids) - 1
		ids[i] = ids[last]
		ids[last] = 0
		ids = ids[:last]
		if s.rrPos[p] > i {
			s.rrPos[p]--
		}
		s.active[p] = ids
		return
	}
}

func (s *proxyScheduler) enqueue(meta muxFrameMeta, frame []byte, pooled bool, seq uint32) {
	if len(frame) == 0 {
		return
	}
	prio := prioP2
	sid := uint16(0)
	if meta.ok {
		sid = meta.sessionID
		prio = classifyXrayFrame(meta)
		if meta.network == 0x02 && meta.dstPort == 53 {
			prio = prioP0
		}
	} else {
		if len(frame) < smallPacketThreshold {
			prio = prioP0
		} else if len(frame) <= 16*1024 {
			prio = prioP1
		}
	}

	it := &schedItem{
		sessionID:   sid,
		prio:        prio,
		enqueued:    time.Now(),
		plain:       frame,
		plainLen:    len(frame),
		plainPooled: pooled,
		seq:         seq,
	}

	s.mu.Lock()
	for !s.closed && s.exceedsLocked(prio, len(frame)) {
		s.cond.Wait()
	}
	if s.closed {
		rel := s.release
		s.mu.Unlock()
		if pooled && rel != nil {
			rel(frame)
		}
		return
	}
	st := s.ensureStreamLocked(sid)
	st.queues[prio] = append(st.queues[prio], it)
	st.items++
	if st.activeMask&(1<<prio) == 0 {
		st.activeMask |= 1 << prio
		s.active[prio] = append(s.active[prio], sid)
	}
	s.backlogBytes += len(frame)
	s.backlogItems++
	urgent := prio == prioP0 && len(frame) < smallPacketThreshold
	notify := s.notify
	s.mu.Unlock()
	if urgent && notify != nil {
		notify()
	}
}

func (s *proxyScheduler) quantum(p schedPriority) int {
	switch p {
	case prioP0:
		return 16 * 1024
	case prioP1:
		return 8 * 1024
	default:
		return 4 * 1024
	}
}

func (s *proxyScheduler) nextFragment(c *RTMPConn, maxFragSize int) ([]byte, schedPriority, time.Time, bool) {
	var (
		st       *schedStream
		selected schedPriority
	)
	s.mu.Lock()
	for {
		if s.closed || c.ctx.Err() != nil {
			s.mu.Unlock()
			return nil, 0, time.Time{}, false
		}
		found := false
		for p := prioP0; p <= prioP2; p++ {
			ids := s.active[p]
			if len(ids) == 0 {
				continue
			}
			for tries := 0; tries < len(ids); tries++ {
				pos := s.rrPos[p]
				if pos >= len(ids) {
					pos = 0
				}
				sid := ids[pos]
				s.rrPos[p] = pos + 1
				st = s.streams[sid]
				if st == nil {
					s.removeActiveLocked(p, sid)
					ids = s.active[p]
					continue
				}
				if st.current != nil {
					if st.current.prio != p {
						continue
					}
				} else if len(st.queues[p]) > 0 {
					st.current = st.queues[p][0]
					st.queues[p][0] = nil
					st.queues[p] = st.queues[p][1:]
				} else {
					s.removeActiveLocked(p, sid)
					st.activeMask &^= 1 << p
					ids = s.active[p]
					continue
				}
				selected = p
				found = true
				break
			}
			if found {
				break
			}
		}
		if !found {
			s.mu.Unlock()
			return nil, 0, time.Time{}, false
		}
		if st.deficit <= 0 {
			st.deficit += s.quantum(selected)
		}
		if st.deficit < crypto.FragmentHeaderSize+1 {
			continue
		}
		break
	}
	it := st.current
	enq := it.enqueued
	first := !it.firstSent
	s.mu.Unlock()

	if it.encrypted == nil {
		if c.cipher != nil {
			enc, err := c.cipher.Encrypt(it.plain)
			if err != nil {
				enc = it.plain
			}
			it.encrypted = enc
		} else {
			it.encrypted = it.plain
		}
		if maxFragSize <= 0 {
			maxFragSize = 1400
		}
		it.totalFrags = (len(it.encrypted) + maxFragSize - 1) / maxFragSize
		if it.totalFrags < 1 {
			it.totalFrags = 1
		}
	}

	fragIndex := it.fragIdx
	start := fragIndex * maxFragSize
	if start > len(it.encrypted) {
		start = len(it.encrypted)
	}
	end := start + maxFragSize
	if end > len(it.encrypted) {
		end = len(it.encrypted)
	}
	payload := it.encrypted[start:end]
	pLen := len(payload)
	buf := make([]byte, crypto.FragmentHeaderSize+pLen)
	h := crypto.PacketFragmentHeader{
		SequenceNum: it.seq,
		FragmentIdx: uint16(fragIndex),
		TotalFrags:  uint16(it.totalFrags),
		PayloadLen:  uint16(pLen),
		Flags:       0,
	}
	h.EncodeTo(buf[:crypto.FragmentHeaderSize])
	copy(buf[crypto.FragmentHeaderSize:], payload)

	done := false
	it.fragIdx++
	if it.fragIdx >= it.totalFrags {
		done = true
	}

	s.mu.Lock()
	st.deficit -= len(buf)
	if first {
		it.firstSent = true
	}
	if done {
		s.backlogBytes -= it.plainLen
		s.backlogItems--
		if s.backlogBytes < 0 {
			s.backlogBytes = 0
		}
		if s.backlogItems < 0 {
			s.backlogItems = 0
		}
		st.current = nil
		if st.items > 0 {
			st.items--
		}
		if it.plainPooled {
			c.putWriteBuf(it.plain)
		}
		it.plain = nil
		it.encrypted = nil
		if len(st.queues[selected]) == 0 {
			s.removeActiveLocked(selected, st.id)
			st.activeMask &^= 1 << selected
		}
		s.cond.Broadcast()
	}
	s.mu.Unlock()
	return buf, selected, enq, first
}

