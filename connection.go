package udpserver

import (
	"net"
)

type udpConnection struct {
	conn *net.UDPConn
	addr *net.UDPAddr
}

// send back the udp socket to the remote peer
func (c *udpConnection) SendTo(buf []byte) error {
	_, err := c.conn.WriteToUDP(buf, c.addr)
	return err
}

// this function is not necessary, but we need this interface
func (c *udpConnection) AsyncWrite(buf []byte) error {
	_, err := c.conn.WriteToUDP(buf, c.addr)
	return err
}

// returns the remote address of this connection
func (c *udpConnection) RemoteAddr() *net.UDPAddr {
	return c.addr
}

// returns the local address of this connection
func (c *udpConnection) LocalAddr() *net.UDPAddr {
	if addr, ok := c.conn.LocalAddr().(*net.UDPAddr); ok {
		return addr
	}
	return nil
}
