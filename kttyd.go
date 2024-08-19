package kttyd

import (
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

type Kttyd struct {
	Port     string
	Path     string
	auth     string
	command  string
	argv     []string
	workdir  string
	title    string
	errlog   func(error)
	listener net.Listener
	srv      *http.Server
}

func randString(n int) string {
	letters := []byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[r.Intn(len(letters))]
	}
	return string(b)
}

func NewKttyd(bindAddress string, command string, args []string) (*Kttyd, error) {
	listener, err := net.Listen("tcp4", bindAddress)
	if err != nil {
		return nil, err
	}

	_, port, _ := net.SplitHostPort(listener.Addr().String())
	path := "/" + randString(16) + "/"
	return &Kttyd{Port: port, Path: path, command: command, argv: args, listener: listener}, nil
}

func (ctx *Kttyd) SetBasicAuth(auth string) *Kttyd {
	ctx.auth = auth
	return ctx
}

func (ctx *Kttyd) SetWorkdir(workdir string) *Kttyd {
	ctx.workdir = workdir
	return ctx
}

func (ctx *Kttyd) SetTitle(title string) *Kttyd {
	ctx.title = title
	return ctx
}

func (ctx *Kttyd) SetLogger(errlog func(error)) *Kttyd {
	ctx.errlog = errlog
	return ctx
}

func CacheControlWrapper(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		h.ServeHTTP(w, r)
	})
}

func BasicAuthWrapper(h http.Handler, credential string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.SplitN(r.Header.Get("Authorization"), " ", 2)

		if len(token) != 2 || strings.ToLower(token[0]) != "basic" {
			w.Header().Set("WWW-Authenticate", `Basic realm="kttyd"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}

		payload, err := base64.StdEncoding.DecodeString(token[1])
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		if credential != string(payload) {
			w.Header().Set("WWW-Authenticate", `Basic realm="kttyd"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}

		h.ServeHTTP(w, r)
	})
}

//go:embed static
var embed_static embed.FS

func (ctx *Kttyd) Start() error {
	ctx.srv = &http.Server{}

	wsHandleFunc := func(w http.ResponseWriter, r *http.Request) {
		upgrader := &websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			Subprotocols:    Protocols,
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		err = ctx.handle(conn, r.Context())
		if err != nil {
			if ctx.errlog != nil {
				ctx.errlog(err)
			}
		}
	}

	fsys, _ := fs.Sub(embed_static, "static")
	http.Handle(ctx.Path, BasicAuthWrapper(CacheControlWrapper(http.StripPrefix(ctx.Path, http.FileServer(http.FS(fsys)))), ctx.auth))
	http.Handle(ctx.Path+"ws", BasicAuthWrapper(http.HandlerFunc(wsHandleFunc), ctx.auth))

	return ctx.srv.Serve(ctx.listener)
}

func (ctx *Kttyd) Stop() {
	ctx.srv.Shutdown(context.Background())
}

type KttydSlave interface {
	Slave

	Close() error
}

type kttydSlave struct {
	command string
	argv    []string

	closeSignal  syscall.Signal
	closeTimeout time.Duration

	cmd       *exec.Cmd
	pty       *os.File
	ptyClosed chan struct{}
}

func (ctx *kttydSlave) Read(p []byte) (n int, err error) {
	return ctx.pty.Read(p)
}

func (ctx *kttydSlave) Write(p []byte) (n int, err error) {
	return ctx.pty.Write(p)
}

func (ctx *kttydSlave) Close() error {
	if ctx.cmd != nil && ctx.cmd.Process != nil {
		ctx.cmd.Process.Signal(ctx.closeSignal)
	}
	for {
		select {
		case <-ctx.ptyClosed:
			return nil
		case <-ctx.closeTimeoutC():
			ctx.cmd.Process.Signal(syscall.SIGKILL)
		}
	}
}

func (ctx *kttydSlave) WindowTitleVariables() map[string]any {
	return map[string]any{
		"command": ctx.command,
		"argv":    ctx.argv,
		"pid":     ctx.cmd.Process.Pid,
	}
}

func (ctx *kttydSlave) ResizeTerminal(width int, height int) error {
	window := pty.Winsize{
		Rows: uint16(height),
		Cols: uint16(width),
		X:    0,
		Y:    0,
	}
	err := pty.Setsize(ctx.pty, &window)
	if err != nil {
		return err
	} else {
		return nil
	}
}

func (ctx *kttydSlave) closeTimeoutC() <-chan time.Time {
	if ctx.closeTimeout >= 0 {
		return time.After(ctx.closeTimeout)
	}

	return make(chan time.Time)
}

func (ctx *Kttyd) newSlave() (KttydSlave, error) {
	cmd := exec.Command(ctx.command, ctx.argv...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if ctx.workdir != "" {
		cmd.Dir = ctx.workdir
	}

	pty, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start command %s, err:%s", ctx.command, err.Error())
	}
	ptyClosed := make(chan struct{})

	slave := &kttydSlave{
		command: ctx.command,
		argv:    ctx.argv,

		closeSignal:  syscall.SIGINT,
		closeTimeout: 10 * time.Second,

		cmd:       cmd,
		pty:       pty,
		ptyClosed: ptyClosed,
	}

	go func() {
		defer func() {
			slave.pty.Close()
			close(slave.ptyClosed)
		}()
		slave.cmd.Wait()
	}()

	return slave, nil
}

type WebsockerMaster struct {
	*websocket.Conn
}

func (ctx *WebsockerMaster) Write(p []byte) (n int, err error) {
	writer, err := ctx.Conn.NextWriter(websocket.TextMessage)
	if err != nil {
		return 0, err
	}
	defer writer.Close()
	return writer.Write(p)
}

func (ctx *WebsockerMaster) Read(p []byte) (n int, err error) {
	for {
		msgType, reader, err := ctx.Conn.NextReader()
		if err != nil {
			return 0, err
		}

		if msgType != websocket.TextMessage {
			continue
		}

		b, err := io.ReadAll(reader)
		if len(b) > len(p) {
			return 0, errors.New("client message exceeded buffer size")
		}
		n = copy(p, b)
		return n, err
	}
}

func (ctx *Kttyd) handle(conn *websocket.Conn, wsctx context.Context) error {
	typ, _, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("failed to authenticate websocket connection, err:%s", err.Error())
	}
	if typ != websocket.TextMessage {
		return errors.New("failed to authenticate websocket connection: invalid message type")
	}

	var slave KttydSlave
	slave, err = ctx.newSlave()
	if err != nil {
		return fmt.Errorf("failed to start command slave, err:%s", err.Error())
	}
	defer slave.Close()

	opts := []Option{
		WithWindowTitle([]byte(ctx.title)),
	}
	opts = append(opts, WithPermitWrite())
	opts = append(opts, WithReconnect(10))
	tty, err := NewWebTTY(&WebsockerMaster{conn}, slave, opts...)
	if err != nil {
		return fmt.Errorf("failed to create webtty, err:%s", err.Error())
	}

	return tty.Run(wsctx)
}
