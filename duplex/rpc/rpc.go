package rpc

import (
	"context"
	"errors"

	"github.com/mitchellh/mapstructure"
	"tractor.dev/toolkit-go/duplex/codec"
	"tractor.dev/toolkit-go/duplex/mux"
)

// A Caller is able to perform remote calls.
//
// Call makes synchronous calls to the remote selector passing args and putting the response
// value(s) in reply. Both args and reply can be nil. Args can be a channel of any
// values for asynchronously streaming multiple values from another goroutine, however
// the call will still block until a response is sent. If there is an error making the call
// an error is returned, and if an error is returned by the remote handler a RemoteError
// is returned. Multiple reply parameters can be provided in order to receive multi-valued
// returns from the remote call.
//
// A Response is also returned for advanced operations. For example, you can check
// if the call is continued, meaning the underlying channel will be kept open for either
// streaming back more results or using the channel as a full duplex byte stream.
type Caller interface {
	Call(ctx context.Context, selector string, params any, reply ...any) (*Response, error)
}

// CallHeader is the first value encoded over the channel to make a call.
type CallHeader struct {
	S string // Selector
}

// Call is used on the responding side of a call and is passed to the handler.
// Call has a Caller so it can be used to make calls back to the calling side.
type Call struct {
	CallHeader

	Caller  Caller
	Decoder codec.Decoder
	Context context.Context

	mux.Channel
}

func (c *Call) Selector() string {
	return c.S
}

// Receive will decode an incoming value from the underlying channel. It can be
// called more than once when multiple values are expected, but should always be
// called once in a handler. It can be called with nil to discard the value.
func (c *Call) Receive(v interface{}) error {
	if v == nil {
		var discard []byte
		v = &discard
	}
	return c.Decoder.Decode(v)
}

// ResponseHeader is the value encoded over the channel to indicate a response.
type ResponseHeader struct {
	E *string // Error
	C bool    // Continue: after parsing response, keep stream open for whatever protocol
}

// Response is used on the calling side to represent a response and allow access
// to the ResponseHeader data, the reply value, the underlying channel, and methods
// to send or receive encoded values over the channel if Continue was set on the
// ResponseHeader.
type Response struct {
	ResponseHeader
	Value   any
	Channel mux.Channel

	codec codec.Codec
}

func (r *Response) Err() error {
	if r.E == nil {
		return nil
	}
	return errors.New(*r.E)
}

func (r *Response) Continue() bool {
	return r.C
}

// Send encodes a value over the underlying channel if it is still open.
func (r *Response) Send(v interface{}) error {
	return r.codec.Encoder(r.Channel).Encode(v)
}

// Receive decodes a value from the underlying channel if it is still open.
func (r *Response) Receive(v interface{}) error {
	return r.codec.Decoder(r.Channel).Decode(v)
}

func (r *Response) Close() error {
	return r.Channel.Close()
}

func (r *Response) CloseWrite() error {
	return r.Channel.CloseWrite()
}

// Responder is used by handlers to initiate a response and send values to the caller.
type Responder interface {
	// Return sends a return value, which can be an error, and closes the channel.
	Return(...any) error

	// Continue sets the response to keep the channel open after sending a return value,
	// and returns the underlying channel for you to take control of. If called, you
	// become responsible for closing the channel.
	Continue(...any) (mux.Channel, error)

	// Send encodes a value over the underlying channel, but does not initiate a response,
	// so it must be used after calling Continue.
	Send(interface{}) error
}

type responder struct {
	responded bool
	header    *ResponseHeader
	ch        mux.Channel
	c         codec.Codec
}

func (r *responder) Send(v interface{}) error {
	return r.c.Encoder(r.ch).Encode(v)
}

func (r *responder) Return(v ...any) error {
	return r.respond(v, false)
}

func (r *responder) Continue(v ...any) (mux.Channel, error) {
	return r.ch, r.respond(v, true)
}

func (r *responder) respond(values []any, continue_ bool) error {
	r.responded = true
	r.header.C = continue_

	// if values is a single error, set values to [nil]
	// and put error in header
	if len(values) == 1 {
		var e error
		var ok bool
		if e, ok = values[0].(error); ok {
			values = []any{nil}
		}
		if e != nil {
			var errStr = e.Error()
			r.header.E = &errStr
		}
	}

	if err := r.Send(r.header); err != nil {
		return err
	}

	// The original calling convention expects at least one return, so return
	// `nil` if there is no other return value.
	if len(values) == 0 {
		values = []any{nil}
	}
	for _, v := range values {
		if err := r.Send(v); err != nil {
			return err
		}
	}

	if !continue_ {
		return r.ch.Close()
	}

	return nil
}

// ReceiveNotify takes a continued response and sends received values to a channel,
// until an error is returned or the context finishes. In either case, the response
// and the channel will be closed.
func ReceiveNotify[T any](ctx context.Context, resp *Response, ch chan T) error {
	defer close(ch)
	defer resp.Close()
	for {
		var v any
		if err := resp.Receive(&v); err != nil {
			return err
		}
		var vv T
		if err := mapstructure.Decode(v, &vv); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		default:
			ch <- vv
		}
	}
}
