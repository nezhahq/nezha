package utils

import (
	"errors"
	"iter"

	"github.com/tidwall/gjson"
)

var (
	ErrGjsonWrongType = errors.New("wrong type")
)

var emptyIterator = func(yield func(string, string) bool) {}

func GjsonIter(json string) (iter.Seq2[string, string], error) {
	if json == "" {
		return emptyIterator, nil
	}

	result := gjson.Parse(json)
	if !result.IsObject() {
		return nil, ErrGjsonWrongType
	}

	return ConvertSeq2(result.ForEach, func(k, v gjson.Result) (string, string) {
		return k.String(), v.String()
	}), nil
}
