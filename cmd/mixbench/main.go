package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shuffleman/rtmp-tunnel/config"
	"github.com/shuffleman/rtmp-tunnel/conn"
	"github.com/shuffleman/rtmp-tunnel/metrics"
)

const (
	xrayStatusNew  = 0x01
	xrayStatusKeep = 0x02

	xrayOptionData = 0x01

	xrayNetTCP = 0x01
	xrayNetUDP = 0x02
)

type xrayParser struct {
	buf []byte
}

func consumeMuxHeader(buf []byte) (need int, enableXray bool, ok bool) {
	if len(buf) < 2 {
		return 0, false, false
	}
	ver := buf[0]
	proto := buf[1]
	if ver > 1 || proto < 1 || proto > 3 {
		return 0, false, false
	}
	if ver == 0 {
		return 2, proto == 3, true
	}
	if len(buf) < 3 {
		return 0, false, false
	}
	paddingEnabled := buf[2] != 0
	if !paddingEnabled {
		return 3, proto == 3, true
	}
	if len(buf) < 5 {
		return 0, false, false
	}
	paddingLen := int(binary.BigEndian.Uint16(buf[3:5]))
	return 5 + paddingLen, proto == 3, true
}

type xrayFrame struct {
	sessionID uint16
	status    byte
	option    byte
	data      []byte
	raw       []byte
}

func (p *xrayParser) push(b []byte) {
	p.buf = append(p.buf, b...)
}

func (p *xrayParser) next() (*xrayFrame, bool) {
	if len(p.buf) < 2 {
		return nil, false
	}
	metaLen := int(binary.BigEndian.Uint16(p.buf[:2]))
	if metaLen <= 0 || metaLen > 512 {
		return nil, false
	}
	if len(p.buf) < 2+metaLen {
		return nil, false
	}
	meta := p.buf[2 : 2+metaLen]
	if len(meta) < 4 {
		return nil, false
	}
	sid := binary.BigEndian.Uint16(meta[0:2])
	status := meta[2]
	option := meta[3]
	pos := 2 + metaLen
	var data []byte
	if option&xrayOptionData != 0 {
		if len(p.buf) < pos+2 {
			return nil, false
		}
		dlen := int(binary.BigEndian.Uint16(p.buf[pos : pos+2]))
		pos += 2
		if len(p.buf) < pos+dlen {
			return nil, false
		}
		data = p.buf[pos : pos+dlen]
		pos += dlen
	}
	raw := p.buf[:pos]
	p.buf = p.buf[pos:]
	return &xrayFrame{sessionID: sid, status: status, option: option, data: data, raw: raw}, true
}

func makeXrayNewFrame(sessionID uint16, network byte, dstPort uint16, payload []byte) []byte {
	meta := make([]byte, 0, 32)
	meta = binary.BigEndian.AppendUint16(meta, sessionID)
	meta = append(meta, xrayStatusNew)
	meta = append(meta, xrayOptionData)
	meta = append(meta, network)
	meta = binary.BigEndian.AppendUint16(meta, dstPort)
	meta = append(meta, 0x01)
	meta = append(meta, 127, 0, 0, 1)

	out := make([]byte, 0, 2+len(meta)+2+len(payload))
	out = binary.BigEndian.AppendUint16(out, uint16(len(meta)))
	out = append(out, meta...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)))
	out = append(out, payload...)
	return out
}

func makeXrayDataFrame(sessionID uint16, payload []byte) []byte {
	meta := make([]byte, 0, 8)
	meta = binary.BigEndian.AppendUint16(meta, sessionID)
	meta = append(meta, xrayStatusKeep)
	meta = append(meta, xrayOptionData)

	out := make([]byte, 0, 2+len(meta)+2+len(payload))
	out = binary.BigEndian.AppendUint16(out, uint16(len(meta)))
	out = append(out, meta...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)))
	out = append(out, payload...)
	return out
}

func p95(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	cp := append([]time.Duration(nil), d...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(math.Ceil(float64(len(cp))*0.95)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func main() {
	var (
		server = flag.String("s", "127.0.0.1:19000", "rtmp benchserver address")
		mode   = flag.String("mode", "A", "A|B")
		sec    = flag.Int("t", 10, "duration seconds")

		dnsQPS    = flag.Int("dnsqps", 200, "dns query qps (scenario A)")
		bulkSz    = flag.Int("bulksz", 64*1024, "bulk payload size")
		bulkMbpsA = flag.Int("bulkmbps", 50, "bulk target Mbps (scenario A, 0=unlimited)")

		conns          = flag.Int("conns", 8, "connections (scenario B)")
		streams        = flag.Int("streams", 16, "streams per connection (scenario B)")
		totalBulkMbpsB = flag.Int("totalbulkmbps", 200, "total bulk target Mbps (scenario B, 0=unlimited)")

		disableSched = flag.Bool("disable-scheduler", false, "disable tunnel scheduler (baseline)")
		metricsAddr  = flag.String("metrics", "", "export metrics on addr (e.g. :9101)")
	)
	flag.Parse()

	if *metricsAddr != "" {
		metrics.Start(*metricsAddr)
		log.Printf("metrics enabled: %s/metrics", *metricsAddr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*sec)*time.Second)
	defer cancel()

	switch *mode {
	case "A", "a":
		runScenarioA(ctx, *server, *dnsQPS, *bulkSz, *bulkMbpsA, *disableSched)
	case "B", "b":
		runScenarioB(ctx, *server, *conns, *streams, *bulkSz, *totalBulkMbpsB, *disableSched)
	default:
		log.Fatalf("unknown mode: %s", *mode)
	}
}

type pacedWriter struct {
	bytesPerSec float64
	next        time.Time
}

func newPacedWriter(mbps int) *pacedWriter {
	if mbps <= 0 {
		return &pacedWriter{bytesPerSec: 0}
	}
	return &pacedWriter{bytesPerSec: float64(mbps) * 1e6 / 8}
}

func (p *pacedWriter) sleepAfterWrite(n int) {
	if p.bytesPerSec <= 0 || n <= 0 {
		return
	}
	d := time.Duration(float64(n) / p.bytesPerSec * float64(time.Second))
	now := time.Now()
	if p.next.IsZero() || p.next.Before(now) {
		p.next = now
	}
	p.next = p.next.Add(d)
	if s := time.Until(p.next); s > 0 {
		time.Sleep(s)
	}
}

func dialRTMP(server string, disableScheduler bool) (net.Conn, error) {
	cfg := config.DefaultConfig()
	cfg.FrameRate = 240
	cfg.TargetBitrate = 500000000
	cfg.MaxSEIPerFrame = 64
	cfg.FlushInterval = 1 * time.Millisecond
	cfg.WriteBatchSize = 256 * 1024
	cfg.TimestampJitterMax = 0
	cfg.SchedulerDisable = disableScheduler
	cfg.Encryption.Enable = true
	cfg.Encryption.Algorithm = "aes-256-gcm"
	cfg.Encryption.Key = []byte("test-secret-key")
	cfg.Validate()

	c := conn.NewClientConn(cfg)
	if err := c.Dial(server); err != nil {
		return nil, err
	}
	return c, nil
}

func runScenarioA(ctx context.Context, server string, dnsQPS int, bulkSize int, bulkMbps int, disableScheduler bool) {
	start := time.Now()
	c, err := dialRTMP(server, disableScheduler)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	var closeOnce sync.Once
	closeConn := func() { closeOnce.Do(func() { _ = c.Close() }) }
	defer closeConn()
	go func() {
		<-ctx.Done()
		closeConn()
	}()

	var (
		parser  xrayParser
		rttMu   sync.Mutex
		pending = make(map[uint16]time.Time)
		rtts    []time.Duration
		wBytes  atomic.Uint64
		rBytes  atomic.Uint64
		nextSID atomic.Uint32
		seenHdr atomic.Bool
	)
	nextSID.Store(1000)

	_, _ = c.Write([]byte{0x00, 0x03})

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 256*1024)
		for {
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			n, err := c.Read(buf)
			_ = c.SetReadDeadline(time.Time{})
			if n > 0 {
				rBytes.Add(uint64(n))
				parser.push(buf[:n])
				if !seenHdr.Load() {
					if need, enableXray, ok := consumeMuxHeader(parser.buf); ok && enableXray && len(parser.buf) >= need {
						parser.buf = parser.buf[need:]
						seenHdr.Store(true)
					}
				}
				for {
					f, ok := parser.next()
					if !ok {
						break
					}
					if f.option&xrayOptionData == 0 {
						continue
					}
					rttMu.Lock()
					t0, ok := pending[f.sessionID]
					if ok {
						delete(pending, f.sessionID)
						rtts = append(rtts, time.Since(t0))
					}
					rttMu.Unlock()
				}
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					select {
					case <-ctx.Done():
						return
					default:
						continue
					}
				}
				return
			}
		}
	}()

	bulkSID := uint16(1)
	_, _ = rand.Read(make([]byte, 1))
	bulkInit := makeXrayNewFrame(bulkSID, xrayNetTCP, 80, []byte("init"))
	_, _ = c.Write(bulkInit)

	bulkPayload := make([]byte, bulkSize)
	rand.Read(bulkPayload)
	go func() {
		pacer := newPacedWriter(bulkMbps)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			frame := makeXrayDataFrame(bulkSID, bulkPayload)
			n, err := c.Write(frame)
			if n > 0 {
				wBytes.Add(uint64(n))
			}
			if err != nil {
				return
			}
			pacer.sleepAfterWrite(n)
		}
	}()

	interval := time.Second
	if dnsQPS > 0 {
		interval = time.Second / time.Duration(dnsQPS)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			closeConn()
			<-readDone
			elapsed := time.Since(start).Seconds()
			if elapsed <= 0 {
				elapsed = 1
			}
			rttMu.Lock()
			p95v := p95(rtts)
			count := len(rtts)
			rttMu.Unlock()
			fmt.Printf("scenario=A disable_scheduler=%v dns_qps=%d bulk_payload=%dB dns_samples=%d dns_p95=%s write=%.2fMbps read=%.2fMbps\n",
				disableScheduler,
				dnsQPS,
				bulkSize,
				count,
				p95v,
				float64(wBytes.Load())*8/elapsed/1e6,
				float64(rBytes.Load())*8/elapsed/1e6,
			)
			return
		case <-ticker.C:
			sid := uint16(nextSID.Add(1))
			payload := make([]byte, 32)
			binary.BigEndian.PutUint16(payload[:2], sid)
			rttMu.Lock()
			pending[sid] = time.Now()
			rttMu.Unlock()
			f := makeXrayNewFrame(sid, xrayNetUDP, 53, payload)
			_, _ = c.Write(f)
		}
	}
}

func runScenarioB(ctx context.Context, server string, conns int, streams int, bulkSize int, totalBulkMbps int, disableScheduler bool) {
	start := time.Now()
	if conns <= 0 {
		conns = 1
	}
	if streams <= 0 {
		streams = 1
	}
	bulkStreamsTotal := conns * ((streams + 3) / 4)
	bulkMbpsPerStream := 0
	if totalBulkMbps > 0 && bulkStreamsTotal > 0 {
		bulkMbpsPerStream = int(math.Max(1, math.Floor(float64(totalBulkMbps)/float64(bulkStreamsTotal))))
	}
	var totalW atomic.Uint64
	var totalR atomic.Uint64
	var rttAllMu sync.Mutex
	var rttAll []time.Duration

	var wg sync.WaitGroup
	wg.Add(conns)
	for i := 0; i < conns; i++ {
		go func(connIdx int) {
			defer wg.Done()
			c, err := dialRTMP(server, disableScheduler)
			if err != nil {
				return
			}
			var closeOnce sync.Once
			closeConn := func() { closeOnce.Do(func() { _ = c.Close() }) }
			defer closeConn()
			go func() {
				<-ctx.Done()
				closeConn()
			}()

			var parser xrayParser
			var pending sync.Map
			var seenHdr atomic.Bool
			localRtts := make([]time.Duration, 0, 2048)
			_, _ = c.Write([]byte{0x00, 0x03})
			readDone := make(chan struct{})
			go func() {
				defer close(readDone)
				buf := make([]byte, 256*1024)
				for {
					_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
					n, err := c.Read(buf)
					_ = c.SetReadDeadline(time.Time{})
					if n > 0 {
						totalR.Add(uint64(n))
						parser.push(buf[:n])
						if !seenHdr.Load() {
							if need, enableXray, ok := consumeMuxHeader(parser.buf); ok && enableXray && len(parser.buf) >= need {
								parser.buf = parser.buf[need:]
								seenHdr.Store(true)
							}
						}
						for {
							f, ok := parser.next()
							if !ok {
								break
							}
							if f.option&xrayOptionData == 0 {
								continue
							}
							if v, ok := pending.LoadAndDelete(f.sessionID); ok {
								localRtts = append(localRtts, time.Since(v.(time.Time)))
							}
						}
					}
					if err != nil {
						if ne, ok := err.(net.Error); ok && ne.Timeout() {
							select {
							case <-ctx.Done():
								return
							default:
								continue
							}
						}
						return
					}
				}
			}()

			baseSID := uint16(1000 + connIdx*1000)
			bulkPayload := make([]byte, bulkSize)
			rand.Read(bulkPayload)
			for s := 0; s < streams; s++ {
				sid := baseSID + uint16(s)
				isBulk := s%4 == 0
				if isBulk {
					_, _ = c.Write(makeXrayNewFrame(sid, xrayNetTCP, 443, []byte("init")))
					go func(sid uint16) {
						pacer := newPacedWriter(bulkMbpsPerStream)
						for {
							select {
							case <-ctx.Done():
								return
							default:
							}
							frame := makeXrayDataFrame(sid, bulkPayload)
							n, err := c.Write(frame)
							if n > 0 {
								totalW.Add(uint64(n))
							}
							if err != nil {
								return
							}
							pacer.sleepAfterWrite(n)
						}
					}(sid)
				} else {
					go func(sid uint16) {
						t := time.NewTicker(20 * time.Millisecond)
						defer t.Stop()
						for {
							select {
							case <-ctx.Done():
								return
							case <-t.C:
								payload := make([]byte, 64)
								pending.Store(sid, time.Now())
								frame := makeXrayNewFrame(sid, xrayNetTCP, 80, payload)
								n, err := c.Write(frame)
								if n > 0 {
									totalW.Add(uint64(n))
								}
								if err != nil {
									return
								}
							}
						}
					}(sid)
				}
			}

			<-ctx.Done()
			closeConn()
			<-readDone
			rttAllMu.Lock()
			rttAll = append(rttAll, localRtts...)
			rttAllMu.Unlock()
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}
	rttAllMu.Lock()
	p95v := p95(rttAll)
	count := len(rttAll)
	rttAllMu.Unlock()
	fmt.Printf("scenario=B disable_scheduler=%v conns=%d streams_per_conn=%d samples=%d small_p95=%s write=%.2fMbps read=%.2fMbps\n",
		disableScheduler,
		conns,
		streams,
		count,
		p95v,
		float64(totalW.Load())*8/elapsed/1e6,
		float64(totalR.Load())*8/elapsed/1e6,
	)
}
