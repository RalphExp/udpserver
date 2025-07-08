package udp

import (
	"net"
)

type udpConnection struct {
	*net.UDPConn
	addr net.Addr // remote addr
}

// send back the udp socket to the remote peer
func (c *udpConnection) SendTo(buf []byte) (int, error) {
	n, err := c.WriteTo(buf, c.addr)
	return n, err
}

// send back the udp socket to the remote peer
func (c *udpConnection) AsyncWrite(buf []byte) (int, error) {
	n, err := c.WriteTo(buf, c.addr)
	return n, err
}

// returns the remote address of this connection
func (c *udpConnection) RemoteAddr() net.Addr {
	return c.addr
}

func (c *udpConnection) Close() error {
	return nil
}
