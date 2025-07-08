package udp

import (
	"context"
	"errors"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panjf2000/ants/v2"
)

type Connection interface {
	// Connection is a UDP-connection interface
	net.PacketConn

	// RemoteAddr returns the remote address of UDPClient/UDPServer
	RemoteAddr() net.Addr
}

type Handler interface {
	// OnDatagram is called when Datagram Server receives a packet
	//
	// Parameters:
	//   - srv: DatagramServer
	//   - conn: Datagram Connection, which can be used to send the packet back to the client
	//   - buf: The packet need to handle, user should not modify the size of buf!
	//   - sz: size of the buffer
	//
	OnDatagram(srv Server, conn Connection, buf []byte, sz int)

	// OnTimer is called every tick interval
	//
	// Parameters:
	//   - srv: DatagramServer, the same as OnDatagram
	OnTimer(srv Server)
}

type Server interface {
	// Serve starts the udp server
	//
	// Parameters:
	//   - addr: server listen address, format is `hostname + ':' + port` e.g. `127.0.0.1:9090`
	//   - handler: a DatagramHandler whose functions will be called when server gets a udp packet
	//
	// Returns:
	//   - err: artc error object
	Serve(addr string, handler Handler) (err error)

	// Stop stops the udp server
	//
	// Returns:
	//   - artc error object
	Stop() error
}

type UDPServer struct {
	ctx        context.Context
	cancel     context.CancelFunc
	bufPool    *sync.Pool
	workerPool *ants.Pool

	workerPoolSize int  // size of ants.Pool
	poolOwner      bool // owner of the ants.Pool

	tick       time.Duration // OnTimer callback interval
	conn       *net.UDPConn  // server socket
	state      atomic.Int32  // server state
	serverAddr *net.UDPAddr  // server address
	logger     *log.Logger

	wg sync.WaitGroup
}

func (s *UDPServer) createWorkerPool() error {
	options := ants.Options{
		ExpiryDuration: antsPoolExpiryDuration,
		Nonblocking:    antsPoolNonblocking,
		PreAlloc:       antsPoolPrealloc,
	}

	pool, err := ants.NewPool(s.workerPoolSize, ants.WithOptions(options),
		ants.WithPanicHandler(func(v interface{}) {
			s.logger.Printf("Task Panic: %v", v)
		}))

	if err != nil {
		return err
	}

	s.workerPool = pool
	return nil
}

func (s *UDPServer) createBufferPool() {
	s.bufPool = &sync.Pool{
		New: func() any {
			return make([]byte, mtuLimit)
		},
	}
}

func (s *UDPServer) getBuffer() []byte {
	return s.bufPool.Get().([]byte)
}

func (s *UDPServer) putBuffer(buf any) {
	s.bufPool.Put(buf)
}

// The init function is safe to called multiple times
func (s *UDPServer) init(address string) (e error) {
	if ok := s.state.CompareAndSwap(serverStopped, serverStarting); !ok {
		return ErrServerAlreadyStarted
	}

	serverAddr, e := net.ResolveUDPAddr("udp", address)
	if e != nil {
		return e
	}

	s.serverAddr = serverAddr
	s.conn, e = net.ListenUDP("udp", s.serverAddr)
	if e != nil {
		s.serverAddr = nil
		return e
	}

	if s.workerPool == nil {
		aerr := s.createWorkerPool()
		if aerr != nil {
			s.conn.Close()
			s.conn = nil
			return aerr
		}
		s.poolOwner = true
	} else {
		s.poolOwner = false
	}

	s.createBufferPool()
	return nil
}

func (s *UDPServer) startTimer(hdl Handler) {
	go func() {
		timer := time.NewTicker(s.tick)
		defer timer.Stop()

		for {
			select {
			case <-s.ctx.Done():
				return
			case <-timer.C:
				hdl.OnTimer(s)
			}
		}
	}()
}

func (s *UDPServer) serverState() int {
	return int(s.state.Load())
}

func (s *UDPServer) Serve(address string, hdl Handler) (err error) {
	if err := s.init(address); err != nil {
		if s.serverState() == serverStarting {
			s.state.Store(serverStopped)
		}
		return err
	}

	s.state.Store(serverStarted)
	s.logger.Printf("[UDPServer]: listen on %s", address)
	defer s.logger.Printf("[UDPServer]: %s stopped", address)

	s.startTimer(hdl)

	go func() {
		<-s.ctx.Done()
		s.Stop()
	}()

	for {
		buf := s.getBuffer()
		n, remote, err := s.conn.ReadFromUDP(buf)
		s.logger.Printf("[UDPServer] recvfrom(%v), %v", remote, buf[:n])

		if err != nil {
			s.putBuffer(buf)
			if errors.Is(err, net.ErrClosed) {
				// probably the socket is closed by Stop()
				return nil
			} else {
				var netErr net.Error
				if errors.As(err, &netErr) && netErr.Timeout() {
					// ignore timeout
					continue
				} else {
					// unknown error, but don't need to restart server
					// just log and continue, see GNET's implementation
					s.logger.Printf("[UDPServer]: recvmsg %s", err.Error())
					continue
				}
			}
		}

		s.wg.Add(1)
		conn := &udpConnection{
			s.conn,
			remote,
		}

		err = s.workerPool.Submit(func() {
			defer s.wg.Done()
			defer s.putBuffer(buf)

			select {
			case <-s.ctx.Done():
				return
			default:
			}

			hdl.OnDatagram(s, conn, buf[:n], n)
		})

		if err != nil {
			s.wg.Done()
			s.putBuffer(buf)
			s.logger.Printf("[UDPServer]: submitTask %s", err.Error())
		}
	}
}

func (s *UDPServer) Send(buf []byte, rmt *net.UDPAddr) (n int, e error) {
	if s.conn == nil {
		return 0, ErrServerNotStart
	}

	n, e = s.conn.WriteToUDP(buf, rmt)
	return
}

// Calling Stop() multiple times should be safe.
func (s *UDPServer) Stop() (err error) {
	if ok := s.state.CompareAndSwap(serverStarted, serverStopping); !ok {
		return ErrServerNotStart
	}

	s.cancel()

	// exit gracefully, wait for all the tasks to finish
	// after canceling the context, only limited new tasks will be
	// delivered to the pool, because if server finds out that
	// the context is done, it will exit. So wg.Wait() will
	// always return without waiting for a long time.
	s.wg.Wait()

	if s.conn != nil {
		// from now on, no new task will use the UDP socket,
		// it's safe to directly close the listen socket,
		// and server will break the loop if it's reading data
		// from the socket, in which case the err is net.ErrClosed
		s.conn.Close()
		s.conn = nil
	}

	// wait again, because when the last Wait() finishes, server may deliver
	// the final task into the pool.
	s.wg.Wait()

	// now no task in the workpool, it's safe to call Release()
	if s.workerPool != nil && s.poolOwner {
		s.workerPool.Release()
		s.workerPool = nil
	}

	s.state.Store(serverStopped)
	return nil
}

func (s *UDPServer) LocalAddr() *net.UDPAddr {
	return s.serverAddr
}

func loadOptions(options ...Option) *Options {
	opts := new(Options)

	for _, option := range options {
		option(opts)
	}
	return opts
}

func (s *UDPServer) checkOptions(opts *Options) error {
	// default ant pool size
	if opts.poolSize <= 0 {
		opts.poolSize = antsPoolDefaultSize
	}

	if opts.tick == 0 {
		opts.tick = time.Second
	} else if opts.tick < time.Millisecond {
		opts.tick = time.Millisecond
	}

	if opts.logger == nil {
		opts.logger = log.Default()
	}

	s.tick = opts.tick
	s.workerPoolSize = opts.poolSize
	if opts.pool != nil {
		s.workerPool = opts.pool
	}

	s.poolOwner = false
	s.logger = opts.logger

	s.logger.Printf("[UDPServer] tick: %v, antSize: %v", s.tick, s.workerPoolSize)
	return nil
}

func NewUDPServer(ctx context.Context, option ...Option) Server {
	subCtx, cancel := context.WithCancel(ctx)
	server := &UDPServer{
		ctx:    subCtx,
		cancel: cancel,
	}
	server.state.Store(serverStopped)
	options := loadOptions(option...)
	server.checkOptions(options)
	return server
}
