package sdk

import (
	"encoding/binary"
	"io"
	"net"
	"time"

	"github.com/frostbyte73/core"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/tinyzimmer/go-gst/gst"
	"github.com/tinyzimmer/go-gst/gst/app"
	"go.uber.org/atomic"

	"github.com/livekit/egress/pkg/errors"
	"github.com/livekit/egress/pkg/types"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go"
	"github.com/livekit/server-sdk-go/pkg/samplebuilder"
)

const (
	maxVideoLate = 1000 // nearly 2s for fhd video
	videoTimeout = time.Second * 2
	maxAudioLate = 200 // 4s for audio
	audioTimeout = time.Second * 4
	maxDropout   = 3000 // max sequence number skip
)

var (
	VP8KeyFrame16x16 = []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0x10, 0x00, 0x10, 0x00, 0x00, 0x47, 0x08, 0x85, 0x85, 0x88, 0x85, 0x84, 0x88, 0x02, 0x02, 0x00, 0x0c, 0x0d, 0x60, 0x00, 0xfe, 0xff, 0xab, 0x50, 0x80}

	H264KeyFrame2x2SPS = []byte{0x67, 0x42, 0xc0, 0x1f, 0x0f, 0xd9, 0x1f, 0x88, 0x88, 0x84, 0x00, 0x00, 0x03, 0x00, 0x04, 0x00, 0x00, 0x03, 0x00, 0xc8, 0x3c, 0x60, 0xc9, 0x20}
	H264KeyFrame2x2PPS = []byte{0x68, 0x87, 0xcb, 0x83, 0xcb, 0x20}
	H264KeyFrame2x2IDR = []byte{0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe}

	H264KeyFrame2x2 = [][]byte{H264KeyFrame2x2SPS, H264KeyFrame2x2PPS, H264KeyFrame2x2IDR}
)

type AppWriter struct {
	logger      logger.Logger
	sb          *samplebuilder.SampleBuilder
	track       *webrtc.TrackRemote
	identity    string
	codec       types.MimeType
	src         *app.Source
	startTime   time.Time
	writeBlanks bool

	newSampleBuilder func() *samplebuilder.SampleBuilder
	writePLI         func()

	// a/v sync
	sync *Synchronizer
	*TrackSynchronizer

	// state
	muted      atomic.Bool
	playing    core.Fuse
	eos        core.Fuse
	eosTimeout time.Duration
	force      core.Fuse
	finished   core.Fuse

	// vp8
	firstPktPushed bool
	vp8Munger      *sfu.VP8Munger
}

func NewAppWriter(
	track *webrtc.TrackRemote,
	rp *lksdk.RemoteParticipant,
	codec types.MimeType,
	src *app.Source,
	sync *Synchronizer,
	syncInfo *TrackSynchronizer,
	writeBlanks bool,
) (*AppWriter, error) {
	w := &AppWriter{
		logger:            logger.GetLogger().WithValues("trackID", track.ID(), "kind", track.Kind().String()),
		track:             track,
		identity:          rp.Identity(),
		codec:             codec,
		src:               src,
		writeBlanks:       writeBlanks,
		sync:              sync,
		TrackSynchronizer: syncInfo,
		playing:           core.NewFuse(),
		eos:               core.NewFuse(),
		finished:          core.NewFuse(),
	}

	var depacketizer rtp.Depacketizer
	var maxLate uint16
	switch codec {
	case types.MimeTypeVP8:
		depacketizer = &codecs.VP8Packet{}
		maxLate = maxVideoLate
		w.eosTimeout = videoTimeout
		w.writePLI = func() { rp.WritePLI(track.SSRC()) }
		w.vp8Munger = sfu.NewVP8Munger(w.logger)

	case types.MimeTypeH264:
		depacketizer = &codecs.H264Packet{}
		maxLate = maxVideoLate
		w.eosTimeout = videoTimeout
		w.writePLI = func() { rp.WritePLI(track.SSRC()) }

	case types.MimeTypeOpus:
		depacketizer = &codecs.OpusPacket{}
		maxLate = maxAudioLate
		w.eosTimeout = audioTimeout

	default:
		return nil, errors.ErrNotSupported(track.Codec().MimeType)
	}

	w.newSampleBuilder = func() *samplebuilder.SampleBuilder {
		return samplebuilder.New(
			maxLate, depacketizer, track.Codec().ClockRate,
			samplebuilder.WithPacketDroppedHandler(w.writePLI),
		)
	}
	w.sb = w.newSampleBuilder()

	go w.run()
	return w, nil
}

// Play marks the track as playing
func (w *AppWriter) Play() {
	w.playing.Break()
}

// SetTrackMuted toggles track mute state
func (w *AppWriter) SetTrackMuted(muted bool) {
	if muted {
		w.logger.Debugw("track muted", "timestamp", time.Since(w.startTime).Seconds())
		w.muted.Store(true)
	} else {
		w.logger.Debugw("track unmuted", "timestamp", time.Since(w.startTime).Seconds())
		w.muted.Store(false)
		if w.writePLI != nil {
			w.writePLI()
		}
	}
}

// EOS blocks until finished
func (w *AppWriter) EOS() {
	w.eos.Break()

	// wait until finished
	<-w.finished.Watch()
}

func (w *AppWriter) run() {
	// always post EOS if the writer started playing
	defer func() {
		if w.playing.IsClosed() {
			_ = w.pushPackets(true)

			if flow := w.src.EndStream(); flow != gst.FlowOK && flow != gst.FlowFlushing {
				w.logger.Errorw("unexpected flow return", nil, "flowReturn", flow.String())
			}
		}

		w.finished.Break()
	}()

	w.startTime = time.Now()

	first := true
	for {
		// read next packet
		_ = w.track.SetReadDeadline(time.Now().Add(time.Millisecond * 500))
		pkt, _, err := w.track.ReadRTP()
		if err != nil {
			if w.eos.IsClosed() {
				return
			} else if isFatalReadError(err) {
				w.logger.Errorw("could not read packet", err)
				return
			}
		}

		if pkt != nil {
			// sync offsets after first packet read
			// see comment in writeRTP below
			if first {
				w.sync.firstPacketForTrack(pkt)
				first = false
			}

			// push packet to sample builder
			w.sb.Push(pkt)

			// push completed packets to appsrc
			err = w.pushPackets(false)
		} else if w.playing.IsOpen() {
			// pipeline not yet playing
			continue
		} else {
			// track was muted, or EOF due to temporary disconnection
			err = w.pushBlankFrames()
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				if w.eos.IsClosed() {
					return
				}
			} else {
				w.logger.Errorw("could not push buffers", err)
				return
			}
		}
	}
}

// pushPackets returns io.EOF once EOS has been received
func (w *AppWriter) pushPackets(force bool) error {
	// buffers can only be pushed to the appsrc while in the playing state
	if w.playing.IsOpen() {
		return nil
	}

	if force {
		return w.push(w.sb.ForcePopPackets(), false)
	} else {
		return w.push(w.sb.PopPackets(), false)
	}
}

// pushBlankFrames returns io.EOF once EOS has been received
func (w *AppWriter) pushBlankFrames() error {
	_ = w.pushPackets(true)

	// TODO: sample buffer has bug that it may pop old packet after pushPackets(true)
	//   recreated it to work now, will remove this when bug fixed
	w.sb = w.newSampleBuilder()

	if !w.writeBlanks {
		// wait until unmuted or closed
		ticker := time.NewTicker(time.Millisecond * 100)
		defer ticker.Stop()

		for {
			<-ticker.C
			if w.eos.IsClosed() || !w.muted.Load() {
				return nil
			}
		}
	}

	// expected difference between packet timestamps
	frameSize := w.getFrameSize()

	// expected packet duration in nanoseconds
	frameDuration := time.Duration(float64(frameSize) * 1e9 / float64(w.track.Codec().ClockRate))
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	for {
		var pkt *rtp.Packet
		var err error

		if !w.muted.Load() {
			_ = w.track.SetReadDeadline(time.Now().Add(frameDuration))
			pkt, _, err = w.track.ReadRTP()
			if err != nil {
				if w.eos.IsClosed() {
					return io.EOF
				} else if isFatalReadError(err) {
					w.logger.Errorw("could not read packet", err)
					return err
				}
			}
		}

		if pkt != nil {
			// once unmuted, next packet determines stopping point
			// the blank frames will be ~500ms behind, and we need to fill the gap
			maxTimestamp := pkt.Timestamp - frameSize
			for {
				ts := w.lastTS + frameSize
				if ts > maxTimestamp {
					// push packet to sample builder and return
					w.sb.Push(pkt)
					return nil
				}

				if err = w.pushBlankFrame(ts); err != nil {
					return err
				}
			}
		} else {
			<-ticker.C
			// push blank frame
			if err = w.pushBlankFrame(w.lastTS + frameSize); err != nil {
				return err
			}
		}
	}
}

// pushBlankFrame returns io.EOF once EOS has been received
func (w *AppWriter) pushBlankFrame(rtpTS uint32) error {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Padding:        false,
			Marker:         true,
			PayloadType:    uint8(w.track.PayloadType()),
			SequenceNumber: w.lastSN + 1,
			Timestamp:      rtpTS,
			SSRC:           uint32(w.track.SSRC()),
			CSRC:           []uint32{},
		},
	}
	w.snOffset++

	switch w.codec {
	case types.MimeTypeVP8:
		blankVP8 := w.vp8Munger.UpdateAndGetPadding(true)

		// 16x16 key frame
		// Used even when closing out a previous frame. Looks like receivers
		// do not care about content (it will probably end up being an undecodable
		// frame, but that should be okay as there are key frames following)
		payload := make([]byte, blankVP8.HeaderSize+len(VP8KeyFrame16x16))
		vp8Header := payload[:blankVP8.HeaderSize]
		err := blankVP8.MarshalTo(vp8Header)
		if err != nil {
			return err
		}

		copy(payload[blankVP8.HeaderSize:], VP8KeyFrame16x16)
		pkt.Payload = payload

	case types.MimeTypeH264:
		buf := make([]byte, 1462)
		offset := 0
		buf[0] = 0x18 // STAP-A
		offset++
		for _, payload := range H264KeyFrame2x2 {
			binary.BigEndian.PutUint16(buf[offset:], uint16(len(payload)))
			offset += 2
			copy(buf[offset:offset+len(payload)], payload)
			offset += len(payload)
		}

		pkt.Payload = buf[:offset]
	}

	if err := w.push([]*rtp.Packet{pkt}, true); err != nil {
		return err
	}

	return nil
}

// push returns io.EOF if EOS has been received
func (w *AppWriter) push(packets []*rtp.Packet, blankFrame bool) error {
	for _, pkt := range packets {
		if !blankFrame {
			// update sequence number
			pkt.SequenceNumber += w.snOffset

			// record max frame size
			if w.lastTS != 0 && pkt.SequenceNumber == w.lastSN+1 {
				if frameSize := pkt.Timestamp - w.lastTS; frameSize > w.frameSize && frameSize < w.clockRate/60 {
					w.frameSize = frameSize
				}
			}

			w.translatePacket(pkt)
		}

		// reset offsets if needed
		if w.lastTS != 0 && pkt.SequenceNumber-w.lastSN > maxDropout && w.lastSN-pkt.SequenceNumber > maxDropout {
			w.logger.Debugw("large SN gap", "previous", w.lastSN, "current", pkt.SequenceNumber)
			w.resetOffsets(pkt)
		}

		// get PTS
		pts, err := w.getPTS(pkt.Timestamp)
		if err != nil {
			return err
		}

		// record timing info
		w.lastSN = pkt.SequenceNumber
		w.lastTS = pkt.Timestamp

		// w.logger.Infow(w.track.Kind().String(), "ts", pkt.Timestamp, "pts", pts, "offset", w.ptsOffset, "sn", pkt.SequenceNumber)

		p, err := pkt.Marshal()
		if err != nil {
			return err
		}

		b := gst.NewBufferFromBytes(p)
		b.SetPresentationTimestamp(pts)
		if ret := w.src.PushBuffer(b); ret != gst.FlowOK {
			w.logger.Infow("flow return", "ret", ret)
		}
	}

	return nil
}

func (w *AppWriter) translatePacket(pkt *rtp.Packet) {
	switch w.codec {
	case types.MimeTypeVP8:
		vp8Packet := buffer.VP8{}
		if err := vp8Packet.Unmarshal(pkt.Payload); err != nil {
			w.logger.Warnw("could not unmarshal VP8 packet", err)
			return
		}

		ep := &buffer.ExtPacket{
			Packet:   pkt,
			Arrival:  time.Now().UnixNano(),
			Payload:  vp8Packet,
			KeyFrame: vp8Packet.IsKeyFrame,
			VideoLayer: buffer.VideoLayer{
				Spatial:  -1,
				Temporal: int32(vp8Packet.TID),
			},
		}

		if !w.firstPktPushed {
			w.firstPktPushed = true
			w.vp8Munger.SetLast(ep)
		} else {
			tpVP8, err := w.vp8Munger.UpdateAndGet(ep, sfu.SequenceNumberOrderingContiguous, ep.Temporal)
			if err != nil {
				w.logger.Warnw("could not update VP8 packet", err)
				return
			}

			payload := pkt.Payload
			payload, err = w.translateVP8Packet(ep.Packet, &vp8Packet, tpVP8.Header, &payload)
			if err != nil {
				w.logger.Warnw("could not translate VP8 packet", err)
				return
			}
			pkt.Payload = payload
		}

	default:
		return
	}
}

func (w *AppWriter) translateVP8Packet(pkt *rtp.Packet, incomingVP8 *buffer.VP8, translatedVP8 *buffer.VP8, outbuf *[]byte) ([]byte, error) {
	var buf []byte
	if outbuf == &pkt.Payload {
		buf = pkt.Payload
	} else {
		buf = (*outbuf)[:len(pkt.Payload)+translatedVP8.HeaderSize-incomingVP8.HeaderSize]

		srcPayload := pkt.Payload[incomingVP8.HeaderSize:]
		dstPayload := buf[translatedVP8.HeaderSize:]
		copy(dstPayload, srcPayload)
	}

	err := translatedVP8.MarshalTo(buf[:translatedVP8.HeaderSize])
	return buf, err
}

func isFatalReadError(err error) bool {
	netErr, isNetErr := err.(net.Error)
	return !errors.Is(err, io.EOF) && (!isNetErr || !netErr.Timeout())
}
