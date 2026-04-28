package conn

import (
	"encoding/binary"
	"sync"
	"time"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/crypto"
	"github.com/shuffleman/rtmp-tunnel/metrics"
)

// proxyScheduler:
// - P0: DNS/CONNECT/TLS handshake/小包(<1KB)
// - P1: 普通请求(<=16KB)
// - P2: bulk(download/video)
// - 调度：P0 抢占；同优先级内按 session RR；每个 session 用 DRR(deficit round robin) 控制公平性
// - 背压：超过 backlog 上限时优先丢 P2，再丢 P1，尽量不丢 P0
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

func (p *muxFrameParser) nextXrayFrame() (muxFrameMeta, []byte, bool) {
	if len(p.buf) < 2 {
		return muxFrameMeta{}, nil, false
	}
	metaLen := int(binary.BigEndian.Uint16(p.buf[:2]))
	if metaLen <= 0 || metaLen > xrayMuxMaxMetadata {
		return muxFrameMeta{}, nil, false
	}
	if len(p.buf) < 2+metaLen {
		return muxFrameMeta{}, nil, false
	}
	meta := p.buf[2 : 2+metaLen]
	if len(meta) < 4 {
		return muxFrameMeta{}, nil, false
	}
	m := muxFrameMeta{
		ok:        true,
		sessionID: binary.BigEndian.Uint16(meta[0:2]),
		status:    meta[2],
		option:    meta[3],
	}
	off := 4
	if m.status == 0x01 {
		if len(meta) < off+1+2 {
			return muxFrameMeta{}, nil, false
		}
		m.network = meta[off]
		off++
		m.dstPort = binary.BigEndian.Uint16(meta[off : off+2])
	}
	pos := 2 + metaLen
	if m.option&0x01 != 0 {
		if len(p.buf) < pos+2 {
			return muxFrameMeta{}, nil, false
		}
		dataLen := int(binary.BigEndian.Uint16(p.buf[pos : pos+2]))
		pos += 2
		if dataLen < 0 || len(p.buf) < pos+dataLen {
			return muxFrameMeta{}, nil, false
		}
		m.dataLen = dataLen
		pos += dataLen
	}
	m.totalLen = pos
	frame := p.buf[:pos]
	p.buf = p.buf[pos:]
	return m, frame, true
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

	isDNS      bool
	activeMask uint8
	items      int
}

type proxyScheduler struct {
	cfg      *config.Config
	disabled bool

	mu     sync.Mutex
	cond   *sync.Cond
	closed bool

	streams map[uint16]*schedStream
	active  [3][]uint16
	rrPos   [3]int

	backlogBytes int
	backlogItems int

	notify        func()
	release       func([]byte)
	onDrop        func(schedPriority)
	onBacklog     func(int)
	onStreamItems func(uint16, int)
}

func newProxyScheduler(cfg *config.Config) *proxyScheduler {
	s := &proxyScheduler{
		cfg:      cfg,
		disabled: cfg.SchedulerDisable,
		streams:  make(map[uint16]*schedStream),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *proxyScheduler) setHooks(notify func(), release func([]byte), onDrop func(schedPriority), onBacklog func(int), onStreamItems func(uint16, int)) {
	s.mu.Lock()
	s.notify = notify
	s.release = release
	s.onDrop = onDrop
	s.onBacklog = onBacklog
	s.onStreamItems = onStreamItems
	s.mu.Unlock()
}

func (s *proxyScheduler) close() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

func (s *proxyScheduler) shedBulk(targetBytes int) {
	if targetBytes < 0 {
		targetBytes = 0
	}
	s.mu.Lock()
	for !s.closed && s.backlogBytes > targetBytes {
		if !s.dropOneLocked(prioP2) {
			break
		}
	}
	s.mu.Unlock()
}

func (s *proxyScheduler) hasPendingLocked() bool {
	if s.backlogItems > 0 {
		return true
	}
	for _, ids := range s.active {
		if len(ids) > 0 {
			return true
		}
	}
	return false
}

func (s *proxyScheduler) hasPending() bool {
	s.mu.Lock()
	ok := s.hasPendingLocked()
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
	if !s.disabled {
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
	lockStart := time.Now()
	if s.closed {
		s.mu.Unlock()
		metrics.ObserveLockHold("scheduler", time.Since(lockStart))
		if pooled && s.release != nil {
			s.release(frame)
		}
		return
	}
	s.enforceLimitsLocked(prio, len(frame))
	if s.shouldDropNewLocked(prio, len(frame)) {
		if s.onDrop != nil {
			s.onDrop(prio)
		}
		rel := s.release
		onBacklog := s.onBacklog
		backlog := s.backlogBytes
		s.mu.Unlock()
		metrics.ObserveLockHold("scheduler", time.Since(lockStart))
		if pooled && rel != nil {
			rel(frame)
		}
		if onBacklog != nil {
			onBacklog(backlog)
		}
		return
	}
	st := s.ensureStreamLocked(sid)
	st.queues[prio] = append(st.queues[prio], it)
	st.items++
	onStreamItems := s.onStreamItems
	if st.activeMask&(1<<prio) == 0 {
		st.activeMask |= 1 << prio
		s.active[prio] = append(s.active[prio], sid)
	}
	s.backlogBytes += len(frame)
	s.backlogItems++
	urgent := prio == prioP0 && len(frame) < smallPacketThreshold
	s.cond.Signal()
	notify := s.notify
	onBacklog := s.onBacklog
	backlog := s.backlogBytes
	items := st.items
	s.mu.Unlock()
	metrics.ObserveLockHold("scheduler", time.Since(lockStart))
	if urgent && notify != nil {
		notify()
	}
	if onBacklog != nil {
		onBacklog(backlog)
	}
	if onStreamItems != nil {
		onStreamItems(sid, items)
	}
}

func (s *proxyScheduler) exceedsLocked(nextPrio schedPriority, nextBytes int) bool {
	if s.cfg.MaxPendingProxyBytes > 0 && s.backlogBytes+nextBytes > s.cfg.MaxPendingProxyBytes {
		return true
	}
	if s.cfg.MaxPendingProxyFragments > 0 && s.backlogItems+1 > s.cfg.MaxPendingProxyFragments {
		return true
	}
	_ = nextPrio
	return false
}

func (s *proxyScheduler) shouldDropNewLocked(p schedPriority, nextBytes int) bool {
	if !s.exceedsLocked(p, nextBytes) {
		return false
	}
	if p == prioP0 {
		return false
	}
	return true
}

func (s *proxyScheduler) enforceLimitsLocked(incomingPrio schedPriority, incomingBytes int) {
	if !s.exceedsLocked(incomingPrio, incomingBytes) {
		return
	}
	for s.exceedsLocked(incomingPrio, incomingBytes) {
		if s.dropOneLocked(prioP2) {
			continue
		}
		if incomingPrio == prioP0 {
			if s.dropOneLocked(prioP1) {
				continue
			}
		}
		break
	}
}

func (s *proxyScheduler) dropOneLocked(p schedPriority) bool {
	ids := s.active[p]
	for _, sid := range ids {
		st := s.streams[sid]
		if st == nil {
			continue
		}
		q := st.queues[p]
		if len(q) == 0 {
			continue
		}
		last := len(q) - 1
		it := q[last]
		q[last] = nil
		st.queues[p] = q[:last]
		s.backlogBytes -= it.plainLen
		s.backlogItems--
		if s.backlogBytes < 0 {
			s.backlogBytes = 0
		}
		if s.backlogItems < 0 {
			s.backlogItems = 0
		}
		if it.plainPooled && s.release != nil {
			s.release(it.plain)
		}
		it.plain = nil
		it.encrypted = nil
		if st.items > 0 {
			st.items--
		}
		if s.onDrop != nil {
			s.onDrop(p)
		}
		if s.onBacklog != nil {
			s.onBacklog(s.backlogBytes)
		}
		if s.onStreamItems != nil {
			s.onStreamItems(sid, st.items)
		}
		if len(st.queues[p]) == 0 && (st.current == nil || st.current.prio != p) {
			s.removeActiveLocked(p, sid)
			st.activeMask &^= 1 << p
		}
		return true
	}
	for sid, st := range s.streams {
		q := st.queues[p]
		if len(q) == 0 {
			continue
		}
		last := len(q) - 1
		it := q[last]
		q[last] = nil
		st.queues[p] = q[:last]
		s.backlogBytes -= it.plainLen
		s.backlogItems--
		if s.backlogBytes < 0 {
			s.backlogBytes = 0
		}
		if s.backlogItems < 0 {
			s.backlogItems = 0
		}
		if it.plainPooled && s.release != nil {
			s.release(it.plain)
		}
		it.plain = nil
		it.encrypted = nil
		if st.items > 0 {
			st.items--
		}
		if s.onDrop != nil {
			s.onDrop(p)
		}
		if s.onBacklog != nil {
			s.onBacklog(s.backlogBytes)
		}
		if s.onStreamItems != nil {
			s.onStreamItems(sid, st.items)
		}
		if len(st.queues[p]) == 0 && (st.current == nil || st.current.prio != p) {
			s.removeActiveLocked(p, sid)
			st.activeMask &^= 1 << p
		}
		return true
	}
	return false
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

// 调度策略：
// - 先保证 P0（DNS/建链/小包）抢占 P1/P2
// - 同一优先级内部使用 DRR（Deficit Round Robin），按 fragment 字节数计费
// - 大流量会被切成多个 fragment，每轮只发送有限额度，避免单一流占满帧预算造成 HOL
func (s *proxyScheduler) nextFragment(c *RTMPConn, maxFragSize int) ([]byte, schedPriority, time.Time, bool) {
	var (
		st       *schedStream
		selected schedPriority
	)
	s.mu.Lock()
	lockStart := time.Now()
	for {
		if s.closed || c.ctx.Err() != nil {
			s.mu.Unlock()
			metrics.ObserveLockHold("scheduler", time.Since(lockStart))
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
			metrics.ObserveLockHold("scheduler", time.Since(lockStart))
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
	metrics.ObserveLockHold("scheduler", time.Since(lockStart))

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
	lockStart2 := time.Now()
	cost := len(buf)
	st.deficit -= cost
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
	}
	onBacklog := s.onBacklog
	onStreamItems := s.onStreamItems
	items := st.items
	sid := st.id
	backlog := s.backlogBytes
	s.mu.Unlock()
	metrics.ObserveLockHold("scheduler", time.Since(lockStart2))

	if onBacklog != nil && done {
		onBacklog(backlog)
	}
	if onStreamItems != nil && done {
		onStreamItems(sid, items)
	}
	return buf, selected, enq, first
}
