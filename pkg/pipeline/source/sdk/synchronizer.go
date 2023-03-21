package sdk

import (
	"io"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/logger"
)

const (
	largePTSDrift         = time.Millisecond * 20
	massivePTSDrift       = time.Second
	maxUInt32       int64 = 4294967296
)

// a single Synchronizer is shared between audio and video writers
type Synchronizer struct {
	sync.RWMutex

	firstPacket   int64
	onFirstPacket func()
	endedAt       int64

	psByIdentity map[string]*participantSynchronizer
	psByTrack    map[uint32]*participantSynchronizer
}

type participantSynchronizer struct {
	sync.Mutex

	ntpStart      time.Time
	syncInfo      map[uint32]*TrackSynchronizer
	senderReports map[uint32]*rtcp.SenderReport
}

type TrackSynchronizer struct {
	sync.Mutex

	trackID  string
	nsPerRTP float64 // nanoseconds per unit increase in RTP timestamp

	firstTS        int64  // first RTP timestamp received
	lastTS         uint32 // most recent RTP timestamp received
	lastTSAdjusted int64  // most recent RTP timestamp, adjusted for uint32 overflow
	rtpStep        uint32 // difference between RTP timestamps for sequential packets (used for blank frame insertion)

	maxPTS    int64 // maximum valid PTS (set after EOS)
	ptsOffset int64 // presentation timestamp offset (used for a/v sync)

	lastSN   uint16 // previous sequence number
	snOffset uint16 // sequence number offset (increases with each blank frame inserted)

	largeDrift time.Duration // track massive PTS drift, in case it's correct
}

func NewSynchronizer(onFirstPacket func()) *Synchronizer {
	return &Synchronizer{
		onFirstPacket: onFirstPacket,
		psByIdentity:  make(map[string]*participantSynchronizer),
		psByTrack:     make(map[uint32]*participantSynchronizer),
	}
}

// addTrack creates track sync info
func (s *Synchronizer) AddTrack(track *webrtc.TrackRemote, identity string) *TrackSynchronizer {
	t := &TrackSynchronizer{
		trackID:        track.ID(),
		nsPerRTP:       float64(1000000000) / float64(track.Codec().ClockRate),
		lastTSAdjusted: -1,
	}
	s.Lock()
	p := s.psByIdentity[identity]
	if p == nil {
		p = &participantSynchronizer{
			syncInfo:      make(map[uint32]*TrackSynchronizer),
			senderReports: make(map[uint32]*rtcp.SenderReport),
		}
		s.psByIdentity[identity] = p
	}
	ssrc := uint32(track.SSRC())
	s.psByTrack[ssrc] = p
	s.Unlock()

	p.Lock()
	p.syncInfo[ssrc] = t
	p.Unlock()

	return t
}

// firstPacketForTrack initializes offsets
func (s *Synchronizer) firstPacketForTrack(pkt *rtp.Packet) {
	now := time.Now().UnixNano()

	s.Lock()
	if s.firstPacket == 0 {
		s.firstPacket = now
		s.onFirstPacket()
	}
	p := s.psByTrack[pkt.SSRC]
	s.Unlock()

	p.Lock()
	t := p.syncInfo[pkt.SSRC]
	p.Unlock()

	t.Lock()
	t.firstTS = int64(pkt.Timestamp)
	t.ptsOffset = now - s.firstPacket
	t.Unlock()
}

func (s *Synchronizer) GetStartedAt() int64 {
	s.RLock()
	defer s.RUnlock()

	return s.firstPacket
}

// OnRTCP syncs a/v using sender reports
func (s *Synchronizer) OnRTCP(packet rtcp.Packet) {
	switch pkt := packet.(type) {
	case *rtcp.SenderReport:
		s.Lock()
		p := s.psByTrack[pkt.SSRC]
		endedAt := s.endedAt
		s.Unlock()

		if endedAt != 0 {
			return
		}

		p.Lock()
		defer p.Unlock()

		p.senderReports[pkt.SSRC] = pkt
		if p.ntpStart.IsZero() {
			if len(p.senderReports) < len(p.syncInfo) {
				// wait for at least one report per track
				return
			}

			// get the max ntp start time for all tracks
			var minNTPStart time.Time
			ntpStarts := make(map[uint32]time.Time)
			for _, report := range p.senderReports {
				t := p.syncInfo[report.SSRC]
				pts, err := t.getPTS(report.RTPTime)
				if err != nil {
					return
				}
				ntpStart := mediatransportutil.NtpTime(report.NTPTime).Time().Add(-pts)
				if minNTPStart.IsZero() || ntpStart.Before(minNTPStart) {
					minNTPStart = ntpStart
				}
				ntpStarts[report.SSRC] = ntpStart
				logger.Infow("Sender report", "RTPTime", report.RTPTime, "PTS", pts, "ntpStart", ntpStart)
			}
			p.ntpStart = minNTPStart

			// update pts delay so all ntp start times match
			for ssrc, ntpStart := range ntpStarts {
				t := p.syncInfo[ssrc]
				if diff := ntpStart.Sub(minNTPStart); diff != 0 {
					t.Lock()
					t.ptsOffset += int64(diff)
					t.Unlock()
				}
			}
		} else {
			p.syncInfo[pkt.SSRC].onSenderReport(pkt, p.ntpStart)
		}
	}
}

// onSenderReport handles pts adjustments for a track
func (t *TrackSynchronizer) onSenderReport(pkt *rtcp.SenderReport, ntpStart time.Time) {
	pts, _ := t.getPTS(pkt.RTPTime)
	expected := mediatransportutil.NtpTime(pkt.NTPTime).Time().Sub(ntpStart)
	if pts != expected {
		diff := expected - pts
		apply := true
		t.Lock()
		if absGreater(diff, largePTSDrift) {
			logger.Warnw("high pts drift", nil, "trackID", t.trackID, "pts", pts, "diff", diff)
			if absGreater(diff, massivePTSDrift) {
				// if it's the first time seeing a massive drift, ignore it
				if t.largeDrift == 0 || absGreater(diff-t.largeDrift, largePTSDrift) {
					t.largeDrift = diff
					apply = false
				}
			}
		}
		if apply {
			t.ptsOffset += int64(diff)
		}
		t.Unlock()
	}
}

func absGreater(value, max time.Duration) bool {
	return value > max || value < -max
}

// getPTS calculates presentation timestamp from RTP timestamp
func (t *TrackSynchronizer) getPTS(rtpTS uint32) (time.Duration, error) {
	t.Lock()
	defer t.Unlock()

	// RTP packet timestamps start at a random number, and increase according to clock rate (for example, with a
	// clock rate of 90kHz, the timestamp will increase by 90000 every second).
	// They can also overflow uint32 and wrap back to 0.
	// The conversion from rtp timestamp to presentation timestamp is done by:
	// 1. Adjusting the rtp timestamp to account for uint32 wrap
	// 2. Get total nanoseconds elapsed since the first packet, according to the clock rate
	// 3. Adding an offset based on sender reports to sync this track with the others

	// adjust timestamp for uint32 wrap if needed
	tsAdjusted := int64(rtpTS)
	if t.lastTSAdjusted != -1 {
		diff := tsAdjusted - t.lastTSAdjusted
		for diff > maxUInt32/2 {
			tsAdjusted -= maxUInt32
			diff -= maxUInt32
		}
		for diff < -maxUInt32/2 {
			tsAdjusted += maxUInt32
			diff += maxUInt32
		}
	}
	t.lastTSAdjusted = tsAdjusted

	// get ns elapsed since the first packet
	nanoSecondsElapsed := int64(float64(tsAdjusted-t.firstTS) * t.nsPerRTP)

	// add offset
	pts := nanoSecondsElapsed + t.ptsOffset

	// if past end time, return EOF
	if t.maxPTS != 0 && pts > t.maxPTS {
		return 0, io.EOF
	}

	return time.Duration(pts), nil
}

// end updates maxPTS
func (s *Synchronizer) End() {
	endTime := time.Now().UnixNano()

	s.Lock()
	defer s.Unlock()

	var maxOffset int64
	for _, p := range s.psByIdentity {
		p.Lock()
		for _, t := range p.syncInfo {
			t.Lock()
			if o := t.ptsOffset; o > maxOffset {
				logger.Infow("new max offset", "offset", o)
				maxOffset = o
			}
			t.Unlock()
		}
		p.Unlock()
	}

	s.endedAt = endTime + maxOffset
	maxPTS := s.endedAt - s.firstPacket

	logger.Infow("Synchronizer End", "endedAt", s.endedAt, "firstPacket", s.firstPacket, "maxOffset", time.Duration(maxOffset), "maxPTS", time.Duration(maxPTS))

	for _, p := range s.psByIdentity {
		p.Lock()
		for _, t := range p.syncInfo {
			t.Lock()
			t.maxPTS = maxPTS
			t.Unlock()
		}
		p.Unlock()
	}
}

func (s *Synchronizer) GetEndedAt() int64 {
	s.RLock()
	defer s.RUnlock()

	return s.endedAt
}
