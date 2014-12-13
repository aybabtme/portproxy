package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"github.com/aybabtme/iocontrol"
	"github.com/dustin/go-humanize"
	"golang.org/x/net/context"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"
)

const timeout = time.Second * 5

var (
	connIDs  = make(chan uint64)
	connDone = make(chan uint64)
)

func main() {
	var (
		lPort        = flag.Int("port", 8080, "local port on which the proxy will listen")
		remoteAddr   = flag.String("raddr", "", "remote address, as a pair of addr:port, where the requests are sent")
		highjackHTTP = flag.Bool("http", false, "usage")
	)
	flag.Parse()

	log.SetFlags(0)
	log.SetPrefix("portproxy: ")

	if *remoteAddr == "" {
		log.Fatal("need to define a remote address")
	}

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", *lPort))
	if err != nil {
		log.Fatalf("couldn't setup listener for proxy: %v", err)
	}
	defer l.Close()

	runtime.GOMAXPROCS(runtime.NumCPU())

	ctx, cancel := context.WithCancel(context.Background())

	log.Printf("now proxying port %d to %q", *lPort, *remoteAddr)
	go func() {
		i := uint64(1)
		var inflight int
		for {
			select {
			case <-ctx.Done():
				return
			case connIDs <- i:
				i++
				inflight++
				log.SetPrefix(fmt.Sprintf("portproxy: %d conns ", inflight))
				log.Printf("[%d] new connection", i)
			case id := <-connDone:
				inflight--
				log.SetPrefix(fmt.Sprintf("portproxy: %d conns ", inflight))
				log.Printf("[%d] connection done", id)
			}
		}
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			cancel()
			log.Fatalf("failed to accept: %v", err)
		}

		if *highjackHTTP {
			go func(conn net.Conn) {
				buf := bytes.NewBuffer(nil)
				brd := bufio.NewReader(io.TeeReader(conn, buf))
				if req, err := http.ReadRequest(brd); err == nil {
					req.RemoteAddr = *remoteAddr
					proxyHTTP(ctx, conn, req)
				} else {
					proxyConn(ctx, conn, buf, *remoteAddr)
				}
			}(conn)

		} else {
			go proxyConn(ctx, conn, nil, *remoteAddr)
		}

	}
}

func proxyHTTP(parent context.Context, lconn net.Conn, req *http.Request) {
	defer lconn.Close()

	start := time.Now()
	id := <-connIDs
	defer func() { connDone <- id }()
	log.Printf("[%d] highjacking HTTP request!", id)

	rconn, err := net.DialTimeout("tcp", req.RemoteAddr, timeout)
	if err != nil {
		log.Printf("[%d] couldn't dial remote address: %v", id, err)
		return
	}
	defer rconn.Close()

	mrd := iocontrol.NewMeasuredReader(rconn)
	mwr := iocontrol.NewMeasuredWriter(rconn)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	go func() {
		tick := time.NewTicker(time.Second)
		for {
			select {
			case <-ctx.Done():
				dur := time.Since(start)
				log.Printf("[%d] %s\tHTTP\ttx:%s @ %sps\t\trx:%s @ %sps",
					id,
					dur,
					humanize.IBytes(uint64(mwr.Total())),
					humanize.IBytes(uint64(mwr.BytesPerSec())),
					humanize.IBytes(uint64(mrd.Total())),
					humanize.IBytes(uint64(mrd.BytesPerSec())),
				)
				return
			case <-tick.C:
			}

			dur := time.Since(start)
			log.Printf("[%d] %s\tHTTP\ttx:%s @ %sps\t\trx:%s @ %sps",
				id,
				dur,
				humanize.IBytes(uint64(mwr.Total())),
				humanize.IBytes(uint64(mwr.BytesPerSec())),
				humanize.IBytes(uint64(mrd.Total())),
				humanize.IBytes(uint64(mrd.BytesPerSec())),
			)
		}
	}()

	if err := req.Write(mwr); err != nil {
		log.Printf("[%d] couldn't write HTTP request: %v", id, err)
		return
	}

	resp, err := http.ReadResponse(bufio.NewReader(mrd), req)
	if err != nil {
		log.Printf("[%d] couldn't read HTTP response: %v", id, err)
		return
	}

	if _, ok := req.Header["Origin"]; ok {
		resp.Header.Set("Access-Control-Allow-Origin", "*")
	}

	if err := resp.Write(lconn); err != nil {
		log.Printf("[%d] couldn't write HTTP response back to client: %v", id, err)
		return
	}
}

func proxyConn(parent context.Context, lconn net.Conn, buf *bytes.Buffer, remoteAddr string) {
	defer lconn.Close()

	id := <-connIDs
	defer func() { connDone <- id }()

	start := time.Now()
	var src io.Reader
	if buf != nil {
		src = io.MultiReader(buf, lconn)
	} else {
		src = lconn
	}
	mrd := iocontrol.NewMeasuredReader(src)
	mwr := iocontrol.NewMeasuredWriter(lconn)

	ctx, cancel := context.WithCancel(parent)

	rconn, err := net.DialTimeout("tcp", remoteAddr, timeout)
	if err != nil {
		log.Printf("couldn't dial remote address: %v", err)
		return
	}
	defer rconn.Close()

	closed := false
	go func() {
		defer cancel()
		for !closed {
			select {
			case <-ctx.Done():
				return
			default:
			}
			lconn.SetReadDeadline(time.Now().Add(timeout))
			rconn.SetWriteDeadline(time.Now().Add(timeout))
			_, err := io.CopyN(rconn, mrd, 8*1<<10)
			if isNormalTerminationError(err) {
				return
			}
			if err != nil {
				log.Printf("[%d] remote address write: %v", id, err)
				return
			}
		}
	}()

	go func() {
		defer cancel()
		for !closed {
			select {
			case <-ctx.Done():
				return
			default:
			}

			rconn.SetReadDeadline(time.Now().Add(timeout))
			lconn.SetWriteDeadline(time.Now().Add(timeout))
			_, err := io.CopyN(mwr, rconn, 8*1<<10)
			if isNormalTerminationError(err) {
				return
			}
			if err != nil {
				log.Printf("[%d] local address write: %v", id, err)
				return
			}
		}
	}()

	tick := time.NewTicker(time.Second)
	for {
		select {
		case <-ctx.Done():
			dur := time.Since(start)
			log.Printf("[%d] %s\tTCP\ttx:%s @ %sps\t\trx:%s @ %sps",
				id,
				dur,
				humanize.IBytes(uint64(mwr.Total())),
				humanize.IBytes(uint64(mwr.BytesPerSec())),
				humanize.IBytes(uint64(mrd.Total())),
				humanize.IBytes(uint64(mrd.BytesPerSec())),
			)
			return
		case <-tick.C:
		}

		dur := time.Since(start)
		log.Printf("[%d] %s\tTCP\ttx:%s @ %sps\t\trx:%s @ %sps",
			id,
			dur,
			humanize.IBytes(uint64(mwr.Total())),
			humanize.IBytes(uint64(mwr.BytesPerSec())),
			humanize.IBytes(uint64(mrd.Total())),
			humanize.IBytes(uint64(mrd.BytesPerSec())),
		)
	}
}

func isNormalTerminationError(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF {
		return true
	}
	e, ok := err.(*net.OpError)
	if ok && e.Timeout() {
		return true
	}

	for _, cause := range []string{
		"use of closed network connection",
		"broken pipe",
		"connection reset by peer",
	} {
		if strings.Contains(err.Error(), cause) {
			return true
		}
	}

	return false
}
