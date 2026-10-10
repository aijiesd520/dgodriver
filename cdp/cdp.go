// Package cdp provides the complete typed Chrome DevTools Protocol snapshot
// shipped with zendriver 0.17.1. Commands execute on a browser or target session.
package cdp

import "context"

//go:generate go run ../internal/cdpgen -spec protocol.json -out .

// Sender is implemented by dgodriver.Connection, Browser and Tab.
type Sender interface {
	Send(context.Context, string, any, any) error
}

type Command[T any] struct {
	Method string
	Params any
}

func (c Command[T]) Execute(ctx context.Context, sender Sender) (T, error) {
	var result T
	err := sender.Send(ctx, c.Method, c.Params, &result)
	return result, err
}

// Ptr distinguishes an omitted optional field from an explicit zero or false.
func Ptr[T any](value T) *T { return &value }

// Event is implemented by each generated event value.
type Event interface{ CDPEventName() string }
