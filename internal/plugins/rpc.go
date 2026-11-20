package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go-bot/plugin"
)

const maxFrame = 1 << 20

var errDisconnected = &plugin.Error{Code: "RUNTIME_DISCONNECTED", Message: "Extension runtime disconnected; an in-flight operation may have an unknown outcome."}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Code       string `json:"code"`
		Name       string `json:"name,omitempty"`
		Classified bool   `json:"classified"`
	} `json:"data"`
}

type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcHandler func(context.Context, string, json.RawMessage) (any, error)

// Each incoming request runs independently: a plugin callback may await a Host
// call while that Host call is itself waiting for the Node runtime. Mutations
// serialize in their owner, never by holding this transport's mutex.
type peer struct {
	ctx      context.Context
	cancel   context.CancelFunc
	conn     net.Conn
	handle   rpcHandler
	writeMu  sync.Mutex
	mu       sync.Mutex
	pending  map[string]chan frame
	active   map[string]context.CancelFunc
	closed   chan struct{}
	once     sync.Once
	sequence atomic.Uint64
}

func newPeer(conn net.Conn, handle rpcHandler) *peer {
	ctx, cancel := context.WithCancel(context.Background())
	p := &peer{ctx: ctx, cancel: cancel, conn: conn, handle: handle, pending: map[string]chan frame{}, active: map[string]context.CancelFunc{}, closed: make(chan struct{})}
	go p.read()
	return p
}

func (p *peer) call(ctx context.Context, method string, input, output any) error {
	return p.callWithDrain(ctx, method, input, output, false)
}

// Tool stages and owned callbacks must settle after cancellation. Ordinary
// management calls may return promptly; a tool result cannot race ahead of
// cleanup still changing the execution world.
func (p *peer) callDrained(ctx context.Context, method string, input, output any) error {
	return p.callWithDrain(ctx, method, input, output, true)
}

func (p *peer) callWithDrain(ctx context.Context, method string, input, output any, drain bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	params, err := json.Marshal(input)
	if err != nil {
		return err
	}
	id := "g" + strconv.FormatUint(p.sequence.Add(1), 10)
	result := make(chan frame, 1)
	p.mu.Lock()
	select {
	case <-p.closed:
		p.mu.Unlock()
		return errDisconnected
	default:
	}
	p.pending[id] = result
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	if err := p.send(frame{ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	decode := func(message frame) error {
		if message.Error != nil {
			return &plugin.Error{Code: message.Error.Data.Code, Name: message.Error.Data.Name, Message: message.Error.Message, Unclassified: !message.Error.Data.Classified}
		}
		if output != nil {
			return json.Unmarshal(message.Result, output)
		}
		return nil
	}
	select {
	case message := <-result:
		return decode(message)
	case <-ctx.Done():
		params, _ := json.Marshal(map[string]string{"id": id})
		go func() { _ = p.send(frame{Method: "request.cancel", Params: params}) }()
		if !drain {
			return ctx.Err()
		}
		// The Node owner has a five-second disposal fence. Allow its failure
		// response to arrive before this transport closes the connection.
		timer := time.NewTimer(6 * time.Second)
		defer timer.Stop()
		select {
		case message := <-result:
			return decode(message)
		case <-p.closed:
			return errDisconnected
		case <-timer.C:
			p.close()
			return failure("PLUGIN_DRAIN_TIMEOUT", "Plugin callback did not settle after cancellation")
		}
	case <-p.closed:
		return errDisconnected
	}
}

func (p *peer) send(message frame) error {
	message.JSONRPC = "2.0"
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(data)+1 > maxFrame {
		return errors.New("extension RPC frame exceeds 1 MiB")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	data = append(data, '\n')
	for len(data) > 0 {
		n, err := p.conn.Write(data)
		if err != nil {
			p.close()
			return errDisconnected
		}
		data = data[n:]
	}
	return nil
}

func (p *peer) read() {
	defer p.close()
	scanner := bufio.NewScanner(p.conn)
	scanner.Buffer(make([]byte, 4096), maxFrame)
	for scanner.Scan() {
		var message frame
		if json.Unmarshal(scanner.Bytes(), &message) != nil || message.JSONRPC != "2.0" {
			return
		}
		if message.Method == "" {
			// A missing result is a malformed response, not a successful void.
			// Void is encoded as result:null; exactly one result/error is required.
			if message.ID == "" || (len(message.Result) > 0) == (message.Error != nil) {
				return
			}
			p.mu.Lock()
			waiting := p.pending[message.ID]
			p.mu.Unlock()
			if waiting != nil {
				select {
				case waiting <- message:
				default:
				}
			}
			continue
		}
		if message.Method == "request.cancel" {
			var params struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(message.Params, &params) == nil {
				p.mu.Lock()
				cancel := p.active[params.ID]
				p.mu.Unlock()
				if cancel != nil {
					cancel()
				}
			}
			continue
		}
		ctx, cancel := context.WithCancel(p.ctx)
		p.mu.Lock()
		if _, duplicate := p.active[message.ID]; duplicate && message.ID != "" {
			p.mu.Unlock()
			cancel()
			return
		}
		if message.ID != "" {
			p.active[message.ID] = cancel
		}
		p.mu.Unlock()
		go p.dispatch(ctx, cancel, message)
	}
}

func (p *peer) dispatch(ctx context.Context, cancel context.CancelFunc, message frame) {
	defer func() { cancel(); p.mu.Lock(); delete(p.active, message.ID); p.mu.Unlock() }()
	result, err := func() (result any, err error) {
		defer func() {
			if value := recover(); value != nil {
				err = fmt.Errorf("Host extension handler panicked: %v", value)
			}
		}()
		if p.handle == nil {
			return nil, &plugin.Error{Code: "METHOD_NOT_FOUND", Message: "Host RPC method is unavailable"}
		}
		return p.handle(ctx, message.Method, message.Params)
	}()
	if message.ID == "" {
		return
	}
	response := frame{ID: message.ID}
	if err == nil {
		response.Result, err = json.Marshal(result)
	}
	if err != nil {
		response.Error = &rpcError{Code: -32000, Message: err.Error()}
		response.Result = nil
		response.Error.Data.Code = "HOST_ERROR"
		var details *plugin.Error
		if errors.As(err, &details) {
			response.Error.Data.Code = details.Code
			response.Error.Data.Name = details.Name
			response.Error.Data.Classified = !details.Unclassified && details.Code != "EXTENSION_ERROR"
		}
		var typed interface{ ToolErrorInfo() (string, string) }
		if errors.As(err, &typed) {
			name, code := typed.ToolErrorInfo()
			if code != "" {
				response.Error.Data.Name = name
				response.Error.Data.Code = code
				response.Error.Data.Classified = true
			}
		}
	}
	if err := p.send(response); err != nil && !errors.Is(err, errDisconnected) {
		fallback := frame{ID: message.ID, Error: &rpcError{Code: -32000, Message: "Host response is not serializable within the RPC size limit"}}
		fallback.Error.Data.Code = "INVALID_RESULT"
		_ = p.send(fallback)
	}
}

func (p *peer) close() {
	p.once.Do(func() {
		close(p.closed)
		p.cancel()
		_ = p.conn.Close()
		p.mu.Lock()
		for _, cancel := range p.active {
			cancel()
		}
		p.mu.Unlock()
	})
}
