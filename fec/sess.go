package fec

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ralphexp/udpserver"
)

const (
	recoveredBufferSize      = 1024
	maxReadQueueSize         = 1024 * 1024
	maxWriteQueueSize        = 1024 * 1024
	clientCloseChanSize      = 1024 // buffered channel
	artcV5HeaderSize         = 6
	maxFECEncodeLatency      = 500 // 500ms
	defaultFECEncoderTimeout = 500 // ms
)

// FEC Client
type FECSession struct {
	conn          net.PacketConn
	fromServer    bool // true if we created conn internally, false if provided by caller
	server        *FECServer
	hdl           udpserver.Handler // udp callback handler
	readQueue     [][]byte
	remote        net.Addr
	fecEncoder    *FecEncoder // if fecEncoder is null, then FEC is disabled
	fecDecoder    *FecDecoder
	readDeadLine  time.Time
	writeDeadLine time.Time
	die           chan struct{}
	dieOnce       sync.Once
	rqLock        sync.Mutex // for readqueu
	decLock       sync.Mutex
	chReadEvent   chan struct{} // notify Read() can be called without blocking
	chWriteEvent  chan struct{} // notify Write() can be called without blocking
}

// FEC Server: base on udpserver
type FECServer struct {
	ctx          context.Context
	cancel       context.CancelFunc
	server       udpserver.Server
	dataShards   int
	parityShards int
	fecHdl       *fecHandler
	sessions     sync.Map
	clientClose  chan string
}

func (sess *FECSession) Role() string {
	if sess.server != nil {
		return "server"
	}
	return "client"
}

// user should provide large enough buffer to receive the data
// otherwise the data will be truncated
func (sess *FECSession) Read(buf []byte) (n int, err error) {
	// TODO: read timeout
	log.Printf("reading data from session")
	for {
		var data []byte
		sess.rqLock.Lock()
		if len(sess.readQueue) > 0 {
			data = sess.readQueue[0]
			sess.readQueue = sess.readQueue[1:]
		}
		sess.rqLock.Unlock()

		if data != nil {
			n = copy(buf, data)
			log.Printf("(%s) read %d bytes, %v", sess.Role(), n, buf[:n])
			return
		}
		select {
		case <-sess.chReadEvent:
			continue
		case <-sess.die:
			return 0, net.ErrClosed
		}
	}
}

func (sess *FECSession) ReadFrom(buf []byte) (n int, addr net.Addr, err error) {
	n, err = sess.Read(buf)
	return n, sess.remote, err
}

func (sess *FECSession) Write(buf []byte) (n int, err error) {
	log.Printf("sendto[%s] %v => %v", sess.Role(), sess.remote.String(), buf)

	// TODO: write timeout
	var fecPacket FecPacket
	fecPacket = make([]byte, len(buf)+FecHeaderSizePlus2)

	binary.LittleEndian.PutUint16(fecPacket[FecHeaderSize:], uint16(len(buf)+2))
	copy(fecPacket[FecHeaderSizePlus2:], buf)

	// now fecEncoder add FEC information to fecPacket!
	parityShards := sess.fecEncoder.Encode(fecPacket, defaultFECEncoderTimeout)
	log.Printf("FecPacket[%s.d]: seqid(%d), flag(%x), data(%v)",
		sess.Role(), fecPacket.Seqid(), fecPacket.Flag(), fecPacket)

	_, err = sess.conn.WriteTo(fecPacket, sess.remote)
	if err != nil {
		log.Printf("[FECSession] WriteTo err: %s", err.Error())
		return 0, err
	}

	log.Printf("parities[%s]: %d", sess.Role(), len(parityShards))
	for _, parity := range parityShards {
		if len(parity) == 0 {
			continue
		}

		fecPacket = parity
		log.Printf("FecPacket[%s.p]: seqid(%d), flag(%x), data(%v)",
			sess.Role(), fecPacket.Seqid(), fecPacket.Flag(), fecPacket)
		_, err := sess.conn.WriteTo(parity, sess.remote)

		if err != nil {
			return n, err
		}
	}
	return len(buf), nil
}

func (sess *FECSession) WriteTo(buf []byte, addr net.Addr) (n int, err error) {
	if addr.String() == sess.remote.String() {
		return sess.Write(buf)
	}
	return 0, fmt.Errorf("address error: %s", addr.String())
}

func (sess *FECSession) SendTo(buf []byte) (n int, err error) {
	return sess.Write(buf)
}

func (sess *FECSession) AsyncWrite(buf []byte) (n int, err error) {
	return sess.Write(buf)
}

// returns the remote address of this connection
func (sess *FECSession) RemoteAddr() net.Addr {
	return sess.remote
}

// returns the local address of this connection
func (sess *FECSession) LocalAddr() net.Addr {
	return sess.conn.LocalAddr()
}

func (sess *FECSession) isClosed() bool {
	select {
	case <-sess.die:
		return true
	default:
		return false
	}
}

func (sess *FECSession) Close() error {
	var once bool
	sess.dieOnce.Do(func() {
		close(sess.die)
		once = true
	})
	if !once {
		return nil
	}

	// puts the buffers back to the pool
	sess.fecDecoder.release()
	atomic.AddUint64(&DefaultSnmp.CurrEstab, ^uint64(0))
	// this is from NewFecSession, so close the socket
	if !sess.fromServer {
		return sess.conn.Close()
	} else {
		// notify server to reclaim the resource
		sess.server.sessions.Delete(sess.remote.String())
	}
	return nil
}

func (sess *FECSession) SetDeadline(t time.Time) error {
	return sess.conn.SetDeadline(t)
}

func (sess *FECSession) SetReadDeadline(t time.Time) error {
	return sess.conn.SetReadDeadline(t)
}

func (sess *FECSession) SetWriteDeadline(t time.Time) error {
	return sess.conn.SetWriteDeadline(t)
}

func (sess *FECSession) readLoop() {
	buf := make([]byte, mtuLimit)
	src := sess.remote.String()
	for {
		if n, addr, err := sess.conn.ReadFrom(buf); err == nil {
			if sess.isClosed() {
				return
			}
			if addr.String() != src {
				atomic.AddUint64(&DefaultSnmp.InErrs, 1)
				continue
			}
			sess.artcInput(buf[:n], n)
		} else {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("[FECSession] readLoop err: %s", err.Error())
		}
	}
}

func (sess *FECSession) artcInput(data []byte, sz int) {
	var fecErrs, fecRecovered, fecParityShards uint64
	fecFlag := binary.LittleEndian.Uint16(data[4:])

	log.Printf("artcInput[%s], flag(%x) %v", sess.Role(), fecFlag, data)
	if fecFlag == TypeData || fecFlag == TypeParity {
		// The header format:
		// | FEC SEQID(4B) | FEC TYPE(2B) | SIZE (2B) | PAYLOAD(SIZE-2) |
		// |<-headerOffset
		var f FecPacket = make([]byte, sz)
		copy(f, data[:sz])
		if f.Flag() == TypeParity {
			fecParityShards++
		}

		sess.decLock.Lock()
		recovers := sess.fecDecoder.Decode(f)
		sess.decLock.Unlock()

		log.Printf("recovered[%s] %d packets", sess.Role(), len(recovers))
		if fecFlag == TypeData {
			if sess.server != nil {
				sess.hdl.OnDatagram(sess.server, sess, data[FecHeaderSizePlus2:], sz-FecHeaderSizePlus2)
			} else {
				log.Printf("append[%s]: flag(%x), %v", sess.Role(), fecFlag, data)
				sess.rqLock.Lock()
				sess.readQueue = append(sess.readQueue, f[FecHeaderSizePlus2:])
				sess.rqLock.Unlock()
				sess.notifyReadEvent()
			}
		}

		// If there're some packets recovered from FEC, feed them into ARTC
		// TODO: so FEC's seqid is lost in recovers?
		for _, r := range recovers {
			log.Printf("recovered[%s]: %v", sess.Role(), r)
			if len(r) >= 2 {
				sz := binary.LittleEndian.Uint16(r)
				if int(sz) <= len(r) && sz >= 2 {
					fecRecovered++
					if sess.server != nil {
						sess.hdl.OnDatagram(sess.server, sess, r[2:sz], int(sz)-2)
					} else {
						log.Printf("append[%s]: %v", sess.Role(), r[2:sz])
						sess.rqLock.Lock()
						sess.readQueue = append(sess.readQueue, r[2:sz])
						sess.rqLock.Unlock()
						sess.notifyReadEvent()
					}
				} else {
					fecErrs++
				}
			} else {
				fecErrs++
			}
		}
	}

	atomic.AddUint64(&DefaultSnmp.InPkts, 1)
	atomic.AddUint64(&DefaultSnmp.InBytes, uint64(len(data)))

	if fecParityShards > 0 {
		atomic.AddUint64(&DefaultSnmp.FECParityShards, fecParityShards)
	}
	if fecErrs > 0 {
		atomic.AddUint64(&DefaultSnmp.FECErrs, fecErrs)
	}
	if fecRecovered > 0 {
		atomic.AddUint64(&DefaultSnmp.FECRecovered, fecRecovered)
	}
}

func (s *FECSession) notifyReadEvent() {
	select {
	case s.chReadEvent <- struct{}{}:
	default:
	}
}

func (s *FECSession) notifyWriteEvent() {
	select {
	case s.chWriteEvent <- struct{}{}:
	default:
	}
}

func newFECSession(dataShards, parityShards int,
	conn net.PacketConn, addr net.Addr, server *FECServer) *FECSession {
	sess := &FECSession{}
	sess.die = make(chan struct{})
	sess.fecEncoder = NewFECEncoder(dataShards, parityShards, 0) // no crypto
	sess.fecDecoder = NewFECDecoder(dataShards, parityShards)
	sess.server = server
	sess.conn = conn
	sess.remote = addr
	sess.chReadEvent = make(chan struct{}, 1)
	sess.chWriteEvent = make(chan struct{}, 1)

	if server != nil { // server
		sess.hdl = server.fecHdl.udpHdl // server.fec must not be nil
		atomic.AddUint64(&DefaultSnmp.ActiveOpens, 1)
	} else { // client
		go sess.readLoop()
		atomic.AddUint64(&DefaultSnmp.PassiveOpens, 1)
	}
	return sess
}

func DialWithOptions(raddr string, dataShards, parityShards int) (*FECSession, error) {
	// network type detection
	rmtaddr, err := net.ResolveUDPAddr("udp", raddr)
	if err != nil {
		return nil, err
	}
	network := "udp4"
	if rmtaddr.IP.To4() == nil {
		network = "udp"
	}

	conn, err := net.ListenUDP(network, nil)
	if err != nil {
		return nil, err
	}

	return newFECSession(dataShards, parityShards, conn, rmtaddr, nil), nil
}

func NewFECServer(ctx context.Context, dataShards, parityShards int, option ...udpserver.Option) *FECServer {
	subCtx, cancel := context.WithCancel(ctx)

	fecServer := &FECServer{
		ctx:          subCtx,
		cancel:       cancel,
		dataShards:   dataShards,
		parityShards: parityShards,
	}

	fecServer.server = udpserver.NewUDPServer(subCtx, option...)
	return fecServer
}

type fecHandler struct {
	server *FECServer
	udpHdl udpserver.Handler
}

func (hdl *fecHandler) OnDatagram(srv udpserver.Server, conn udpserver.Connection, data []byte, sz int) {
	log.Printf("[FECHandler] recvfrom: %v => (%v), %v\n", conn.RemoteAddr().String(), data, sz)

	// check the length first
	if len(data) < artcV5HeaderSize {
		// bad packet
		log.Printf("drop packet %v", data)
		atomic.AddUint64(&DefaultSnmp.ARTCInErrors, 1)
		return
	}

	server := hdl.server
	sess, ok := server.sessions.Load(conn)

	var fecPacket FecPacket = data
	fecFlag := fecPacket.Flag()

	if fecFlag != TypeData && fecFlag != TypeParity {
		log.Printf("Non FEC packet detected: %v", data[:sz])
		return
	}

	if !ok { // no existing connection, create a new one
		sess = newFECSession(server.dataShards, server.parityShards, conn, conn.RemoteAddr(), server)
		sess, _ = server.sessions.LoadOrStore(conn.RemoteAddr().String(), sess)
	}
	sess.(*FECSession).artcInput(fecPacket[:sz], sz)
}

func (hdl *fecHandler) OnTimer(srv udpserver.Server) {
	if hdl.udpHdl != nil {
		hdl.udpHdl.OnTimer(srv)
	}
}

func (s *FECServer) Serve(address string, hdl udpserver.Handler) error {
	s.clientClose = make(chan string, clientCloseChanSize)
	s.fecHdl = &fecHandler{
		server: s,
		udpHdl: hdl,
	}

	go func() {
		for {
			select {
			case <-s.ctx.Done():
				s.Stop()
				return
			case rmt := <-s.clientClose:
				s.sessions.Delete(rmt)
			}
		}
	}()

	return s.server.Serve(address, s.fecHdl)
}

func (s *FECServer) Stop() error {
	if s.clientClose != nil {
		close(s.clientClose)
		s.clientClose = nil
	}
	res := s.server.Stop()
	s.cancel()
	return res
}
