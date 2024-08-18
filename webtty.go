package kttyd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

var Protocols = []string{"webtty"}

const (
	// Unknown message type, maybe sent by a bug
	UnknownInput = '0'
	// User input typically from a keyboard
	Input = '1'
	// Ping to the server
	Ping = '2'
	// Notify that the browser size has been changed
	ResizeTerminal = '3'
	// Change encoding
	SetEncoding = '4'
)

const (
	// Unknown message type, maybe set by a bug
	UnknownOutput = '0'
	// Normal output to the terminal
	Output = '1'
	// Pong to the browser
	Pong = '2'
	// Set window title of the terminal
	SetWindowTitle = '3'
	// Set terminal preference
	SetPreferences = '4'
	// Make terminal to reconnect
	SetReconnect = '5'
	// Set the input buffer size
	SetBufferSize = '6'
)

var (
	// ErrSlaveClosed indicates the function has exited by the slave
	ErrSlaveClosed = errors.New("slave closed")

	// ErrSlaveClosed is returned when the slave connection is closed.
	ErrMasterClosed = errors.New("master closed")
)

type Master io.ReadWriter

type Slave interface {
	io.ReadWriter

	// WindowTitleVariables returns any values that can be used to fill out
	// the title of a terminal.
	WindowTitleVariables() map[string]any

	// ResizeTerminal sets a new size of the terminal.
	ResizeTerminal(columns int, rows int) error
}

type Decoder interface {
	Decode(dst, src []byte) (int, error)
}

type Encoder interface {
	Encode(dst, src []byte) (int, error)
}

type NullCodec struct{}

func (NullCodec) Encode(dst, src []byte) (int, error) {
	return copy(dst, src), nil
}

func (NullCodec) Decode(dst, src []byte) (int, error) {
	return copy(dst, src), nil
}

type WebTTY struct {
	// PTY Master, which probably a connection to browser
	masterConn Master
	// PTY Slave
	slave Slave

	windowTitle []byte
	permitWrite bool
	columns     int
	rows        int
	reconnect   int // in seconds
	masterPrefs []byte
	decoder     Decoder

	bufferSize int
	writeMutex sync.Mutex
}

// Option is an option for WebTTY.
type Option func(*WebTTY) error

// WithPermitWrite sets a WebTTY to accept input from slaves.
func WithPermitWrite() Option {
	return func(wt *WebTTY) error {
		wt.permitWrite = true
		return nil
	}
}

// WithFixedColumns sets a fixed width to TTY master.
func WithFixedColumns(columns int) Option {
	return func(wt *WebTTY) error {
		wt.columns = columns
		return nil
	}
}

// WithFixedRows sets a fixed height to TTY master.
func WithFixedRows(rows int) Option {
	return func(wt *WebTTY) error {
		wt.rows = rows
		return nil
	}
}

// WithWindowTitle sets the default window title of the session
func WithWindowTitle(windowTitle []byte) Option {
	return func(wt *WebTTY) error {
		wt.windowTitle = windowTitle
		return nil
	}
}

// WithReconnect enables reconnection on the master side.
func WithReconnect(timeInSeconds int) Option {
	return func(wt *WebTTY) error {
		wt.reconnect = timeInSeconds
		return nil
	}
}

// WithMasterPreferences sets an optional configuration of master.
func WithMasterPreferences(preferences any) Option {
	return func(wt *WebTTY) error {
		prefs, err := json.Marshal(preferences)
		if err != nil {
			return errors.New("failed to marshal preferences as JSON")
		}
		wt.masterPrefs = prefs
		return nil
	}
}

// New creates a new instance of WebTTY.
// masterConn is a connection to the PTY master,
// typically it's a websocket connection to a client.
// slave is a PTY slave such as a local command with a PTY.
func NewWebTTY(masterConn Master, slave Slave, options ...Option) (*WebTTY, error) {
	wt := &WebTTY{
		masterConn: masterConn,
		slave:      slave,

		permitWrite: false,
		columns:     0,
		rows:        0,

		bufferSize: 1024,
		decoder:    &NullCodec{},
	}

	for _, option := range options {
		option(wt)
	}

	return wt, nil
}

// Run starts the main process of the WebTTY.
// This method blocks until the context is canceled.
// Note that the master and slave are left intact even
// after the context is canceled. Closing them is caller's
// responsibility.
// If the connection to one end gets closed, returns ErrSlaveClosed or ErrMasterClosed.
func (wt *WebTTY) Run(ctx context.Context) error {
	err := wt.sendInitializeMessage()
	if err != nil {
		return fmt.Errorf("failed to send initializing message, err:%s", err.Error())
	}

	errs := make(chan error, 2)

	go func() {
		errs <- func() error {
			buffer := make([]byte, wt.bufferSize)
			for {
				//base64 length
				effectiveBufferSize := wt.bufferSize - 1
				//max raw data length
				maxChunkSize := int(effectiveBufferSize/4) * 3

				n, err := wt.slave.Read(buffer[:maxChunkSize])
				if err != nil {
					return ErrSlaveClosed
				}

				err = wt.handleSlaveReadEvent(buffer[:n])
				if err != nil {
					return err
				}
			}
		}()
	}()

	go func() {
		errs <- func() error {
			buffer := make([]byte, wt.bufferSize)
			for {
				n, err := wt.masterConn.Read(buffer)
				if err != nil {
					return ErrMasterClosed
				}

				err = wt.handleMasterReadEvent(buffer[:n])
				if err != nil {
					return err
				}
			}
		}()
	}()

	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errs:
	}

	return err
}

func (wt *WebTTY) sendInitializeMessage() error {
	err := wt.masterWrite(append([]byte{SetWindowTitle}, wt.windowTitle...))
	if err != nil {
		return fmt.Errorf("failed to send window title, err:%s", err.Error())
	}

	bufSizeMsg, _ := json.Marshal(wt.bufferSize)
	err = wt.masterWrite(append([]byte{SetBufferSize}, bufSizeMsg...))
	if err != nil {
		return fmt.Errorf("failed to send buffer size, err:%s", err.Error())
	}

	if wt.reconnect > 0 {
		reconnect, _ := json.Marshal(wt.reconnect)
		err := wt.masterWrite(append([]byte{SetReconnect}, reconnect...))
		if err != nil {
			return fmt.Errorf("failed to set reconnect, err:%s", err.Error())
		}
	}

	if wt.masterPrefs != nil {
		err := wt.masterWrite(append([]byte{SetPreferences}, wt.masterPrefs...))
		if err != nil {
			return fmt.Errorf("failed to set preferences, err:%s", err.Error())
		}
	}

	return nil
}

func (wt *WebTTY) handleSlaveReadEvent(data []byte) error {
	safeMessage := base64.StdEncoding.EncodeToString(data)
	err := wt.masterWrite(append([]byte{Output}, []byte(safeMessage)...))
	if err != nil {
		return fmt.Errorf("failed to send message to master, err:%s", err.Error())
	}

	return nil
}

func (wt *WebTTY) masterWrite(data []byte) error {
	wt.writeMutex.Lock()
	defer wt.writeMutex.Unlock()

	_, err := wt.masterConn.Write(data)
	if err != nil {
		return fmt.Errorf("failed to write to master, err:%s", err.Error())
	}

	return nil
}

type argResizeTerminal struct {
	Columns float64
	Rows    float64
}

func (wt *WebTTY) handleMasterReadEvent(data []byte) error {
	if len(data) == 0 {
		return errors.New("unexpected zero length read from master")
	}

	switch data[0] {
	case Input:
		if !wt.permitWrite {
			return nil
		}

		if len(data) <= 1 {
			return nil
		}

		var decodedBuffer = make([]byte, len(data))
		n, err := wt.decoder.Decode(decodedBuffer, data[1:])
		if err != nil {
			return fmt.Errorf("failed to decode received data, err:%s", err.Error())
		}

		_, err = wt.slave.Write(decodedBuffer[:n])
		if err != nil {
			return fmt.Errorf("failed to write received data to slave, err:%s", err.Error())
		}

	case Ping:
		err := wt.masterWrite([]byte{Pong})
		if err != nil {
			return fmt.Errorf("failed to return Pong message to master, err:%s", err.Error())
		}

	case SetEncoding:
		switch string(data[1:]) {
		case "base64":
			wt.decoder = base64.StdEncoding
		case "null":
			wt.decoder = NullCodec{}
		}

	case ResizeTerminal:
		if wt.columns != 0 && wt.rows != 0 {
			break
		}

		if len(data) <= 1 {
			return errors.New("received malformed remote command for terminal resize: empty payload")
		}

		var args argResizeTerminal
		err := json.Unmarshal(data[1:], &args)
		if err != nil {
			return fmt.Errorf("received malformed data for terminal resize, err:%s", err.Error())
		}
		rows := wt.rows
		if rows == 0 {
			rows = int(args.Rows)
		}

		columns := wt.columns
		if columns == 0 {
			columns = int(args.Columns)
		}

		wt.slave.ResizeTerminal(columns, rows)
	default:
		return fmt.Errorf("unknown message type `%c`", data[0])
	}

	return nil
}
