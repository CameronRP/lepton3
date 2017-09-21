package lepton3

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"time"

	tomb "gopkg.in/tomb.v2"
	"periph.io/x/periph/conn/spi"
	"periph.io/x/periph/conn/spi/spireg"
)

// XXX deal with printfs (enable a debug mode or something?)
// XXX document copy minimisation
// XXX allow speed to be selected
// XXX measure error rate over time

const (
	// Video Over SPI packets
	vospiHeaderSize = 4 // 2 byte ID, 2 byte CRC
	vospiDataSize   = 160
	vospiPacketSize = vospiHeaderSize + vospiDataSize

	// Packets, segments and frames
	colsPerFrame      = 160
	rowsPerFrame      = 120
	packetsPerSegment = 60
	segmentsPerFrame  = 4
	packetsPerFrame   = segmentsPerFrame * packetsPerSegment
	colsPerPacket     = colsPerFrame / 2
	segmentPacketNum  = 20
	maxPacketNum      = 59

	// SPI transfer
	packetsPerRead     = 200 // XXX play around with this to check effect on CPU load and reliability
	transferSize       = vospiPacketSize * packetsPerRead
	packetBufferSize   = 1024
	maxPacketsPerFrame = 1500 // including discards and then rounded up somewhat

	// Packet bitmasks
	packetHeaderDiscard = 0x0F
	packetNumMask       = 0x0FFF

	// The maximum time a single frame read is allowed to take
	// (including resync attempts)
	frameTimeout = 10 * time.Second
)

// New returns a new Lepton3 instance.
func New() *Lepton3 {
	// The ring buffer is used to avoid memory allocations for SPI
	// transfers. It needs to be big enough to handle all the SPI
	// transfers for at least a single frame.
	ringChunks := int(math.Ceil(float64(maxPacketsPerFrame) / float64(packetsPerRead)))
	return &Lepton3{
		ring:  newRing(ringChunks, transferSize),
		frame: newFrame(),
	}
}

// Lepton3 manages a connection to an FLIR Lepton 3 camera. It is not
// goroutine safe.
type Lepton3 struct {
	spiPort  spi.PortCloser
	spiConn  spi.Conn
	packetCh chan []byte
	tomb     *tomb.Tomb
	ring     *ring
	frame    *frame
}

// Open initialises the SPI connection and starts streaming packets
// from the camera.
func (d *Lepton3) Open() error {
	spiPort, err := spireg.Open("")
	if err != nil {
		return err
	}
	spiConn, err := spiPort.Connect(30000000, spi.Mode3, 8)
	if err != nil {
		spiPort.Close()
		return err
	}

	d.spiPort = spiPort
	d.spiConn = spiConn

	return d.startStream()
}

// Close stops streaming of packets from the camera and closes the SPI
// device connection. It must only be called if streaming was started
// with Open().
func (d *Lepton3) Close() {
	d.stopStream()
	if d.spiPort != nil {
		d.spiPort.Close()
	}
	d.spiConn = nil
}

// NextFrame returns the next frame from the camera into the image
// provided.
//
// The output image is provided (rather than being created by
// NextFrame) to minimise memory allocations. Use NewFrameImage() to
// create an image suitable for use with NextFrame().
//
// NextFrame should only be called after a successful call to
// Open(). Although there is some internal buffering of camera
// packets, NextFrame must be called frequently enough to ensure
// frames are not lost.
func (d *Lepton3) NextFrame(im *image.Gray16) error {
	timeout := time.After(frameTimeout)
	d.frame.reset()

	var packet []byte
	for {
		select {
		case packet = <-d.packetCh:
		case <-d.tomb.Dying():
			if err := d.tomb.Err(); err != nil {
				return fmt.Errorf("streaming failed with: %v", err)
			}
			return nil
		case <-timeout:
			return errors.New("frame timeout")
		}

		packetNum, err := validatePacket(packet)
		if err != nil {
			fmt.Println(err)
			if err := d.resync(); err != nil {
				return err
			}
			continue
		} else if packetNum < 0 {
			continue
		}

		complete, err := d.frame.nextPacket(packetNum, packet)
		if err != nil {
			fmt.Printf("addPacket: %v\n", err)
			if err := d.resync(); err != nil {
				return err
			}
		} else if complete {
			d.frame.writeImage(im)
			return nil
		}
	}
}

// Snapshot is convenience method for capturing a single frame. It
// should *not* be called if streaming is already active.
func (d *Lepton3) Snapshot() (*image.Gray16, error) {
	if err := d.Open(); err != nil {
		return nil, err
	}
	defer d.Close()
	im := NewFrameImage()
	if err := d.NextFrame(im); err != nil {
		return nil, err
	}
	return im, nil
}

func (d *Lepton3) resync() error {
	fmt.Println("resync!")
	d.Close()
	d.frame.reset()
	time.Sleep(300 * time.Millisecond)
	return d.Open()
}

func (d *Lepton3) startStream() error {
	if d.tomb != nil {
		return errors.New("streaming already active")
	}
	d.tomb = new(tomb.Tomb)
	d.packetCh = make(chan []byte, packetBufferSize)
	d.tomb.Go(func() error {
		for {
			rx := d.ring.next()
			if err := d.spiConn.Tx(nil, rx); err != nil {
				return err
			}
			for i := 0; i < len(rx); i += vospiPacketSize {
				if rx[i]&packetHeaderDiscard == packetHeaderDiscard {
					// No point sending discard packets onwards.
					// This makes a big difference to CPU utilisation.
					continue
				}
				select {
				case <-d.tomb.Dying():
					return tomb.ErrDying
				case d.packetCh <- rx[i : i+vospiPacketSize]:
				}
			}
		}
	})
	return nil
}

func (d *Lepton3) stopStream() {
	d.tomb.Kill(nil)
	d.tomb.Wait()
	d.tomb = nil
}

func validatePacket(packet []byte) (int, error) {
	header := binary.BigEndian.Uint16(packet)
	if header&0x8000 == 0x8000 {
		return -1, errors.New("first bit set on header")
	}

	packetNum := int(header & packetNumMask)
	if packetNum > 60 {
		return -1, errors.New("invalid packet number")
	}

	// XXX might not necessary with CRC check
	if packetNum == 0 && packet[2] == 0 && packet[3] == 0 {
		return -1, nil
	}

	// XXX CRC checks

	return packetNum, nil
}
