package utils

import (
	"encoding"
	"reflect"

	"github.com/go-viper/mapstructure/v2"
	"sigs.k8s.io/yaml"
)

// TextUnmarshalerHookFunc is a fixed version of mapstructure.TextUnmarshallerHookFunc.
// This hook allows to additionally unmarshal text into custom string types that implement the encoding.Text(Un)Marshaler interface(s).
func TextUnmarshalerHookFunc() mapstructure.DecodeHookFuncType {
	return func(
		f reflect.Type,
		t reflect.Type,
		data any,
	) (any, error) {
		if f.Kind() != reflect.String {
			return data, nil
		}
		result := reflect.New(t).Interface()
		unmarshaller, ok := result.(encoding.TextUnmarshaler)
		if !ok {
			return data, nil
		}

		// default text representation is the actual value of the `from` string
		var (
			dataVal = reflect.ValueOf(data)
			text    = []byte(dataVal.String())
		)
		if f.Kind() == t.Kind() {
			// source and target are of underlying type string
			var (
				err    error
				ptrVal = reflect.New(dataVal.Type())
			)
			if !ptrVal.Elem().CanSet() {
				// cannot set, skip, this should not happen
				if err := unmarshaller.UnmarshalText(text); err != nil {
					return nil, err
				}
				return result, nil
			}
			ptrVal.Elem().Set(dataVal)

			// We need to assert that both, the value type and the pointer type
			// do (not) implement the TextMarshaller interface before proceeding and simply
			// using the string value of the string type.
			// it might be the case that the internal string representation differs from
			// the (un)marshalled string.

			for _, v := range []reflect.Value{dataVal, ptrVal} {
				if marshaller, ok := v.Interface().(encoding.TextMarshaler); ok {
					text, err = marshaller.MarshalText()
					if err != nil {
						return nil, err
					}
					break
				}
			}
		}

		// text is either the source string's value or the source string type's marshaled value
		// which may differ from its internal string value.
		if err := unmarshaller.UnmarshalText(text); err != nil {
			return nil, err
		}
		return result, nil
	}
}

// KubeYAML implements a YAML parser.
type KubeYAML struct{}

// Unmarshal parses the given YAML bytes.
func (p *KubeYAML) Unmarshal(b []byte) (map[string]any, error) {
	var out map[string]any
	if err := yaml.Unmarshal(b, &out); err != nil {
		return nil, err
	}

	return out, nil
}

// Marshal marshals the given config map to YAML bytes.
func (p *KubeYAML) Marshal(o map[string]any) ([]byte, error) {
	return yaml.Marshal(o)
}
