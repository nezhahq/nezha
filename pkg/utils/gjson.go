package utils

import (
	"errors"
	"iter"

	"github.com/tidwall/gjson"
)

var (
	ErrGjsonNotFound  = errors.New("specified path does not exist")
	ErrGjsonWrongType = errors.New("wrong type")
)

var emptyIterator = func(yield func(string, string) bool) {}

func GjsonGet(json []byte, path string) (gjson.Result, error) {
	result := gjson.GetBytes(json, path)
	if !result.Exists() {
		return result, ErrGjsonNotFound
	}

	return result, nil
}

func GjsonIter(json string) (iter.Seq2[string, string], error) {
	if json == "" {
		return emptyIterator, nil
	}

	result := gjson.Parse(json)
	if !result.IsObject() {
		return nil, ErrGjsonWrongType
	}

	return func(yield func(string, string) bool) {
		result.ForEach(func(k, v gjson.Result) bool {
			return yield(k.String(), v.String())
		})
	}, nil
}
