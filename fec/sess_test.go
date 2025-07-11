package fec

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ralphexp/udp"
)

var baseport = uint32(10000)

type echoHandler struct{}

func (hdl *echoHandler) OnTimer(server udp.Server) {}
func (hdl *echoHandler) OnDatagram(server udp.Server, conn udp.Connection, buf []byte, sz int) {
	// echo to client, note that conn is a FECSession!!!

	log.Printf("******recvfrom %v: %v******\n", conn.RemoteAddr().String(), buf[:sz])
	conn.WriteTo(buf, conn.RemoteAddr())
}

func echoServer(port int) *FECServer {
	server := NewFECServer(context.TODO(), 4, 2)
	hdl := echoHandler{}
	go func() {
		server.Serve(fmt.Sprintf("127.0.0.1:%v", port), &hdl)
	}()

	return server
}

func dialEcho(port int) (*FECSession, error) {
	sess, err := DialWithOptions(fmt.Sprintf("127.0.0.1:%v", port), 4, 2)
	if err != nil {
		panic(err)
	}
	return sess, err
}

func TestSendRecv(t *testing.T) {
	port := int(atomic.AddUint32(&baseport, 1))
	re := regexp.MustCompile(`HelloWorldARTCv5(\d+)`)

	server := echoServer(port)
	defer server.Stop()
	// make sure server runs
	time.Sleep(time.Millisecond * 100)

	cli, err := dialEcho(port)
	if err != nil {
		panic(err)
	}

	const N = 100

	buf := make([]byte, 24)
	for i := range N {
		msg := fmt.Sprintf("HelloWorldARTCv5%v", i)
		cli.Write([]byte(msg))
		for {
			if n, err := cli.Read(buf); err == nil {
				m := re.FindStringSubmatch(string(buf[:n]))
				if len(m) != 2 {
					t.Errorf(`expected HelloWorldARTCv5\d but got %s`, buf[:n])
					t.Fail()
					break
				}
				if d, _ := strconv.Atoi(m[1]); d == i {
					t.Logf("match %s!", buf[:n])
					break
				} else {
					t.Logf("got %s", buf[:n])
				}
			} else {
				panic(err)
			}
		}
	}
	cli.Close()
}
