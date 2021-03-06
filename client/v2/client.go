package v2

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"

	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zlib"

	protocol "github.com/elastic/go-lumber/protocol/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

// Client implements the low-level lumberjack wire protocol. SyncClient and
// AsyncClient should be used for publishing events to lumberjack endpoint.
type Client struct {
	conn net.Conn
	wb   *bytes.Buffer

	opts options
}

var (
	codeWindowSize    = []byte{protocol.CodeVersion, protocol.CodeWindowSize}
	codeCompressed    = []byte{protocol.CodeVersion, protocol.CodeCompressed}
	codeJSONDataFrame = []byte{protocol.CodeVersion, protocol.CodeJSONDataFrame}

	empty4 = []byte{0, 0, 0, 0}
)

var (
	// ErrProtocolError is returned if an protocol error was detected in the
	// conversation with lumberjack server.
	ErrProtocolError = errors.New("lumberjack protocol error")
	loggers          log.Logger
)

var lock sync.Mutex

// NewWithConn create a new lumberjack client with an existing and active
// connection.
func NewWithConn(c net.Conn, opts ...Option) (*Client, error) {
	o, err := applyOptions(opts)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn: c,
		wb:   bytes.NewBuffer(nil),
		opts: o,
	}, nil
}

// Dial connects to the lumberjack server and returns new Client.
// Returns an error if connection attempt fails.
func Dial(address string, opts ...Option) (*Client, error) {
	o, err := applyOptions(opts)
	if err != nil {
		return nil, err
	}

	//get PortRange logger

	portRange := o.portRange
	loggers = o.logger

	//set local port
	port := getAvailablePortInRange(portRange)
	var dialer net.Dialer
	if port != 0 {
		netAddr := &net.TCPAddr{Port: port}
		dialer = net.Dialer{Timeout: o.timeout, LocalAddr: netAddr}
	} else {
		//cant't find port ,discard task
		return nil, fmt.Errorf("can't find port to create client")
	}

	return DialWith(dialer.Dial, address, opts...)
}

//get available port in range

func getAvailablePortInRange(portRange string) int {
	lock.Lock()
	defer lock.Unlock()
	if portRange == "" {
		return GetAvailablePort()
	}
	var startPort, endPort, port int
	regexStr := "^\\d+,(\\s+)?\\d+$"
	reg, _ := regexp.Compile(regexStr)
	if reg.MatchString(portRange) {
		startPort, _ = strconv.Atoi(strings.TrimSpace(strings.Split(portRange, ",")[0]))
		endPort, _ = strconv.Atoi(strings.TrimSpace(strings.Split(portRange, ",")[1]))
	} else {
		level.Error(loggers).Log("msg", "client_port_range format error", " client_port_range: ", portRange)
		return 0
	}

	if startPort < 30000 {
		startPort = 30000
	}
	if endPort > 65535 {
		endPort = 65535
	}
	if startPort > endPort {
		level.Error(loggers).Log("msg", "client_port_range invalid", "client_port_range:", portRange)
	}
	counter := make(map[int]bool)
	//sleep 10msec to  prevent multiple clients from connecting too quickly to the same port randomly
	time.Sleep(10 * time.Millisecond)
	rand.Seed(time.Now().UnixNano())
	for {
		if len(counter) >= (endPort - startPort) {
			break
		}
		port = rand.Intn(endPort-startPort) + startPort
		counter[port] = true
		if IsPortAvailable(port) {
			level.Info(loggers).Log("msg", "find port in range", "port:", port, " range: ", portRange)
			return port
		}
	}
	level.Error(loggers).Log("msg", "cant't find port in range", " range: ", portRange)
	return 0
}

//get available port in local
func GetAvailablePort() int {
	address, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:0", "0.0.0.0"))
	if err != nil {
		return 0
	}
	listener, err := net.ListenTCP("tcp", address)
	if err != nil {
		return 0
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port

}

//is port available
func IsPortAvailable(port int) bool {
	address := fmt.Sprintf("%s:%d", "0.0.0.0", port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		level.Info(loggers).Log("msg", "port was used", " port: ", port)
		return false
	}
	defer listener.Close()
	return true
}

// DialWith uses provided dialer to connect to lumberjack server returning a
// new Client. Returns error if connection attempt fails.
func DialWith(
	dial func(network, address string) (net.Conn, error),
	address string,
	opts ...Option,
) (*Client, error) {
	c, err := dial("tcp", address)
	if err != nil {
		return nil, err
	}

	client, err := NewWithConn(c, opts...)
	if err != nil {
		_ = c.Close() // ignore error
		return nil, err
	}
	return client, nil
}

// Close closes underlying network connection
func (c *Client) Close() error {
	return c.conn.Close()
}

// Send attempts to JSON-encode and send all events without waiting for ACK.
// Returns error if sending or serialization fails.
func (c *Client) Send(data []interface{}) error {
	if len(data) == 0 {
		return nil
	}

	// 1. create window message
	c.wb.Reset()
	_, _ = c.wb.Write(codeWindowSize)
	writeUint32(c.wb, uint32(len(data)))

	// 2. serialize data (payload)
	if c.opts.compressLvl > 0 {
		// Compressed Data Frame:
		// version: uint8 = '2'
		// code: uint8 = 'C'
		// payloadSz: uint32
		// payload: compressed payload

		_, _ = c.wb.Write(codeCompressed) // write compressed header

		offSz := c.wb.Len()
		_, _ = c.wb.Write(empty4)
		offPayload := c.wb.Len()

		// compress payload
		w, err := zlib.NewWriterLevel(c.wb, c.opts.compressLvl)
		if err != nil {
			return err
		}

		if err := c.serialize(w, data); err != nil {
			return err
		}

		if err := w.Close(); err != nil {
			return err
		}

		// write compress header
		payloadSz := c.wb.Len() - offPayload
		binary.BigEndian.PutUint32(c.wb.Bytes()[offSz:], uint32(payloadSz))
	} else {
		if err := c.serialize(c.wb, data); err != nil {
			return err
		}
	}

	// 3. send buffer
	if err := c.setWriteDeadline(); err != nil {
		return err
	}
	payload := c.wb.Bytes()
	for len(payload) > 0 {
		n, err := c.conn.Write(payload)
		if err != nil {
			return err
		}

		payload = payload[n:]
	}

	return nil
}

// ReceiveACK awaits and reads next ACK response or error. Note: Server might
// send partial ACK, in which case client must continue reading ACKs until last send
// window size is matched. Use AwaitACK when waiting for a known sequence number.
func (c *Client) ReceiveACK() (uint32, error) {
	if err := c.setReadDeadline(); err != nil {
		return 0, err
	}

	var msg [6]byte
	ackbytes := 0
	for ackbytes < 6 {
		n, err := c.conn.Read(msg[ackbytes:])
		if err != nil {
			return 0, err
		}
		ackbytes += n
	}

	// validate response
	isACK := msg[0] == protocol.CodeVersion && msg[1] == protocol.CodeACK
	if !isACK {
		return 0, ErrProtocolError
	}

	seq := binary.BigEndian.Uint32(msg[2:])
	return seq, nil
}

// AwaitACK waits for count elements being ACKed. Returns last known ACK on error.
func (c *Client) AwaitACK(count uint32) (uint32, error) {
	var ackSeq uint32
	var err error

	// read until all acks
	for ackSeq < count {
		ackSeq, err = c.ReceiveACK()
		if err != nil {
			return ackSeq, err
		}
	}

	if ackSeq > count {
		return count, fmt.Errorf(
			"invalid sequence number received (seq=%v, expected=%v)", ackSeq, count)
	}
	return ackSeq, nil
}

func (c *Client) serialize(out io.Writer, data []interface{}) error {
	for i, d := range data {
		b, err := c.opts.encoder(d)
		if err != nil {
			return err
		}

		// Write JSON Data Frame:
		// version: uint8 = '2'
		// code: uint8 = 'J'
		// seq: uint32
		// payloadLen (bytes): uint32
		// payload: JSON document

		_, _ = out.Write(codeJSONDataFrame)
		writeUint32(out, uint32(i)+1)
		writeUint32(out, uint32(len(b)))
		_, _ = out.Write(b)
	}
	return nil
}

func (c *Client) setWriteDeadline() error {
	return c.conn.SetWriteDeadline(time.Now().Add(c.opts.timeout))
}

func (c *Client) setReadDeadline() error {
	return c.conn.SetReadDeadline(time.Now().Add(c.opts.timeout))
}

func (c *Client) GetLocalAddr() string {
	return c.conn.LocalAddr().String()
}

func writeUint32(out io.Writer, v uint32) {
	_ = binary.Write(out, binary.BigEndian, v)
}
