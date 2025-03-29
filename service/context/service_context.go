package context

import (
	"context"
)

type Context struct {
	context.Context
}

type contextKey[T any, PT interface{ *T }] struct {
	ptr PT
}

func NewContext(ctx context.Context) *Context {
	return &Context{ctx}
}

func WithValue[T any, PT interface{ *T }](parent *Context, val PT) *Context {
	var nilptr PT
	key := contextKey[T, PT]{nilptr}

	valCtx := context.WithValue(parent.Context, key, val)
	return &Context{valCtx}
}

func Value[T any, PT interface{ *T }](ctx *Context) PT {
	var nilptr PT
	key := contextKey[T, PT]{nilptr}

	val, _ := ctx.Value(key).(PT)
	return val
}
