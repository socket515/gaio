package ev

import (
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"testing"
)

func init() {

	go http.ListenAndServe(":6060", nil)
}

func echoServer(t testing.TB) net.Listener {
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	w, err := CreateWatcher()
	if err != nil {
		t.Fatal(err)
	}

	rx := make([]byte, 128)
	tx := make([]byte, 128)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Println(err)
				return
			}

			fd, err := w.Watch(conn)
			if err != nil {
				log.Println(err)
				return
			}

			log.Println("watching", conn.RemoteAddr(), "fd:", fd)

			onReadComplete := func(req *Request) {
				if req.NBytes > 0 {
					//log.Println("oncomplete:", req.Fd, req.NBytes, string(req.Buffer[:req.NBytes]))
					writeRequest := Request{
						Fd:         fd,
						Buffer:     tx,
						NBytes:     req.NBytes,
						OnComplete: func(req *Request) {},
					}
					w.Write(&writeRequest)
				}
			}

			readRequest := Request{
				Fd:          fd,
				Buffer:      rx,
				ReadPersist: true,
				OnComplete:  onReadComplete,
			}

			err = w.Read(&readRequest)
			if err != nil {
				log.Println(err)
				return
			}
		}
	}()
	return ln
}

func TestEcho(t *testing.T) {
	ln := echoServer(t)
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tx := []byte("hello world")
	rx := make([]byte, len(tx))

	conn.Write(tx)
	t.Log("tx:", string(tx))
	_, err = conn.Read(rx)
	if err != nil {
		t.Fatal(err)
	}

	t.Log("rx:", string(tx))
}

func BenchmarkEcho(b *testing.B) {
	ln := echoServer(b)

	addr, _ := net.ResolveTCPAddr("tcp", ln.Addr().String())
	tx := []byte("hello world")
	rx := make([]byte, len(tx))

	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		b.Fatal(err)
		return
	}

	b.ResetTimer()
	b.SetBytes(int64(len(tx)))
	for i := 0; i < b.N; i++ {
		conn.Write(tx)
		conn.Read(rx)
	}
}
