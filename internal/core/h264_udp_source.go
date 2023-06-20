package core

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"time"

	"github.com/bluenviron/gortsplib/v3/pkg/formats"
	"github.com/bluenviron/gortsplib/v3/pkg/media"
	"github.com/bluenviron/mediacommon/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/pkg/formats/mpegts"
	"golang.org/x/net/ipv4"

	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/formatprocessor"
	"github.com/bluenviron/mediamtx/internal/logger"
)

/* func opusGetPacketDuration(pkt []byte) time.Duration {
	if len(pkt) == 0 {
		return 0
	}

	frameDuration := opusDurations[pkt[0]>>3]

	frameCount := 0
	switch pkt[0] & 3 {
	case 0:
		frameCount = 1
	case 1:
		frameCount = 2
	case 2:
		frameCount = 2
	case 3:
		if len(pkt) < 2 {
			return 0
		}
		frameCount = int(pkt[1] & 63)
	}

	return (time.Duration(frameDuration) * time.Duration(frameCount) * time.Millisecond) / 48
} */

/* func joinMulticastGroupOnAtLeastOneInterface(p *ipv4.PacketConn, listenIP net.IP) error {
	intfs, err := net.Interfaces()
	if err != nil {
		return err
	}

	success := false

	for _, intf := range intfs {
		if (intf.Flags & net.FlagMulticast) != 0 {
			err := p.JoinGroup(&intf, &net.UDPAddr{IP: listenIP})
			if err == nil {
				success = true
			}
		}
	}

	if !success {
		return fmt.Errorf("unable to activate multicast on any network interface")
	}

	return nil
} */

type packetH264ConnReader struct {
	pc        net.PacketConn
	midbuf    []byte
	midbufpos int
}

func newPacketH264ConnReader(pc net.PacketConn) *packetH264ConnReader {
	return &packetH264ConnReader{
		pc:     pc,
		midbuf: make([]byte, 0, 1500),
	}
}

func (r *packetH264ConnReader) Read(p []byte) (int, error) {
	if r.midbufpos < len(r.midbuf) {
		n := copy(p, r.midbuf[r.midbufpos:])
		r.midbufpos += n
		return n, nil
	}

	mn, _, err := r.pc.ReadFrom(r.midbuf[:cap(r.midbuf)])
	if err != nil {
		return 0, err
	}

	if (mn % 188) != 0 {
		return 0, fmt.Errorf("received packet with size %d not multiple of 188", mn)
	}

	r.midbuf = r.midbuf[:mn]
	n := copy(p, r.midbuf)
	r.midbufpos = n
	return n, nil
}

type h264udpSourceParent interface {
	logger.Writer
	sourceStaticImplSetReady(req pathSourceStaticSetReadyReq) pathSourceStaticSetReadyRes
	sourceStaticImplSetNotReady(req pathSourceStaticSetNotReadyReq)
}

type h264udpSource struct {
	readTimeout conf.StringDuration
	parent      h264udpSourceParent
}

func newH264UDPSource(
	readTimeout conf.StringDuration,
	parent h264udpSourceParent,
) *h264udpSource {
	return &h264udpSource{
		readTimeout: readTimeout,
		parent:      parent,
	}
}

func (s *h264udpSource) Log(level logger.Level, format string, args ...interface{}) {
	s.parent.Log(level, "[udp source] "+format, args...)
}

// run implements sourceStaticImpl.
func (s *h264udpSource) run(ctx context.Context, cnf *conf.PathConf, _ chan *conf.PathConf) error {
	s.Log(logger.Debug, "connecting")

	hostPort := cnf.Source[len("h264://"):]

	pc, err := net.ListenPacket(restrictNetwork("udp", hostPort))
	if err != nil {
		return err
	}
	defer pc.Close()

	host, _, _ := net.SplitHostPort(hostPort)
	ip := net.ParseIP(host)

	if ip.IsMulticast() {
		p := ipv4.NewPacketConn(pc)

		err = p.SetMulticastTTL(multicastTTL)
		if err != nil {
			return err
		}

		err = joinMulticastGroupOnAtLeastOneInterface(p, ip)
		if err != nil {
			return err
		}
	}

	/* 	dem := astits.NewDemuxer(
	context.Background(),
	newPacketH264ConnReader(pc),
	astits.DemuxerOptPacketSize(188)) */

	readerErr := make(chan error)
	var timedec *mpegts.TimeDecoder
	go func() {
		readerErr <- func() error {
			pc.SetReadDeadline(time.Now().Add(time.Duration(s.readTimeout)))

			/* 			tmp := uint64(buf[8])<<56 | uint64(buf[7])<<48 | uint64(buf[6])<<40 | uint64(buf[5])<<32 |
				uint64(buf[4])<<24 | uint64(buf[3])<<16 | uint64(buf[2])<<8 | uint64(buf[1])
			dts := time.Duration(tmp) * time.Microsecond

			nalus, err := h264.AnnexBUnmarshal(buf[9:]) */

			//tracks, err := mpegts.FindTracks(dem)
			if err != nil {
				return err
			}

			var medias media.Medias
			mediaCallbacks := make(map[uint16]func(time.Duration, []byte), 1)
			var stream *stream
			var medi *media.Media
			medi = &media.Media{
				Type: media.TypeVideo,
				Formats: []formats.Format{&formats.H264{
					PayloadTyp:        96,
					PacketizationMode: 1,
				}},
			}

			mediaCallbacks[12345] = func(pts time.Duration, data []byte) {

				au, err := h264.AnnexBUnmarshal(data)
				//au, err := h264.AVCCUnmarshal(data)

				if err != nil {
					s.Log(logger.Warn, "%v", err)
					return
				}

				stream.writeUnit(medi, medi.Formats[0], &formatprocessor.UnitH264{
					PTS: pts,
					AU:  au,
					NTP: time.Now(),
				})
			}
			medias = append(medias, medi)

			res := s.parent.sourceStaticImplSetReady(pathSourceStaticSetReadyReq{
				medias:             medias,
				generateRTPPackets: true,
			})
			if res.err != nil {
				return res.err
			}

			defer s.parent.sourceStaticImplSetNotReady(pathSourceStaticSetNotReadyReq{})

			s.Log(logger.Info, "ready: %s", sourceMediaInfo(medias))

			stream = res.stream
			var pts time.Duration

			startH264Header := [3]byte{0x00, 0x00, 0x01}
			//var start int = 0
			//var end int = 0
			cb, _ := mediaCallbacks[12345]
			/* if !ok {
				continue
			} */
			counter := 0
			packetBuffer := make([]byte, (0))
			for {
				pc.SetReadDeadline(time.Now().Add(time.Duration(s.readTimeout)))
				input := make([]byte, (1024 * 10))
				n, _, err := pc.ReadFrom(input[0:])

				if err != nil {
					return err
				}

				//cb(pts, input[0:n])
				//continue

				//split := bytes.Split(packetBuffer[0:], startH264Header[0:])
				//s.Log(logger.Info, "split count %d", len(split))
				packetBuffer = append(packetBuffer, input[0:n]...)
				lastIndex := bytes.LastIndex(packetBuffer[0:], startH264Header[0:])
				if lastIndex > 1 {
					if timedec == nil {
						timedec = mpegts.NewTimeDecoder(time.Now().UnixMilli())
						pts = 0
					} else {
						//pts = timedec.Decode(time.Now().UnixMicro())
						//pts = time.Duration(1*counter+1/30*90000) * time.Millisecond
					}
					//pts = timedec.Decode(time.Now().UnixMilli())
					s.Log(logger.Info, "pts %d", pts)
					cb(pts, packetBuffer[0:lastIndex])
					//cb(time.Duration(counter)*time.Millisecond, packetBuffer[0:lastIndex])
					counter++
					packetBuffer = packetBuffer[lastIndex:]
					pts = timedec.Decode(time.Now().UnixMilli())
				} else {
					//s.Log(logger.Info, "skipping")
				}
				//s.Log(logger.Info, "len %d", len(packetBuffer))
				/* copy(packetBuffer[end:], input[0:n])
				end += n
				cb(pts, packetBuffer[0:end]) */

				/* var data *DemuxerData
				//data, err := dem.NextData()


				if data.PES == nil {
					continue
				}

				if data.PES.Header.OptionalHeader == nil ||
					data.PES.Header.OptionalHeader.PTSDTSIndicator == astits.PTSDTSIndicatorNoPTSOrDTS ||
					data.PES.Header.OptionalHeader.PTSDTSIndicator == astits.PTSDTSIndicatorIsForbidden {
					return fmt.Errorf("PTS is missing")
				}

				var pts time.Duration
				if timedec == nil {
					timedec = mpegts.NewTimeDecoder(data.PES.Header.OptionalHeader.PTS.Base)
					pts = 0
				} else {
					pts = timedec.Decode(data.PES.Header.OptionalHeader.PTS.Base)
				}

				cb, ok := mediaCallbacks[data.PID]
				if !ok {
					continue
				} */

				//cb(pts, data.PES.Data)
			}
		}()
	}()

	select {
	case err := <-readerErr:
		return err

	case <-ctx.Done():
		pc.Close()
		<-readerErr
		return fmt.Errorf("terminated")
	}
}

// apiSourceDescribe implements sourceStaticImpl.
func (*h264udpSource) apiSourceDescribe() pathAPISourceOrReader {
	return pathAPISourceOrReader{
		Type: "h264udpSource",
		ID:   "",
	}
}
