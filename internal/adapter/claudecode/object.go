package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// object is a JSON object that remembers the order its keys arrived in.
//
// Go's encoding/json decodes an object into a map, and a map re-encodes
// with its keys sorted. Against a config the user maintains by hand that
// turns every install into a whole-file reordering: a diff that touches
// lines we never meant to change, and a file the user no longer
// recognises. Uninstall could not then restore what it found.
//
// So objects keep an ordered slice of keys alongside their values. New
// keys append, which puts our additions at the end and leaves every
// existing line where it was.
type object struct {
	keys   []string
	values map[string]any
}

// newObject returns an empty object.
func newObject() *object {
	return &object{values: map[string]any{}}
}

// Get returns the value for a key.
func (o *object) Get(key string) (any, bool) {
	if o == nil || o.values == nil {
		return nil, false
	}
	v, ok := o.values[key]
	return v, ok
}

// Set stores a value, appending the key if it is new and keeping its
// existing position if it is not.
func (o *object) Set(key string, value any) {
	if o.values == nil {
		o.values = map[string]any{}
	}
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

// Delete removes a key, preserving the order of the rest.
func (o *object) Delete(key string) {
	if o == nil || o.values == nil {
		return
	}
	if _, exists := o.values[key]; !exists {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Keys returns the keys in their original order. The result is a copy, so
// a caller may delete while ranging over it.
func (o *object) Keys() []string {
	if o == nil {
		return nil
	}
	out := make([]string, len(o.keys))
	copy(out, o.keys)
	return out
}

// Len reports how many keys the object holds.
func (o *object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

// UnmarshalJSON decodes an object while recording key order. Nested
// objects and arrays are decoded through the same path, so ordering is
// preserved at every depth.
func (o *object) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected a JSON object, got %v", tok)
	}

	o.keys = nil
	o.values = map[string]any{}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("expected an object key, got %v", keyTok)
		}
		value, err := decodeValue(dec)
		if err != nil {
			return err
		}
		o.Set(key, value)
	}

	// Consume the closing brace.
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

// decodeValue reads the next value, turning objects into *object and
// arrays into []any so that nesting keeps its order too.
func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}

	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		// A scalar: string, json.Number, bool or nil.
		return tok, nil
	}

	switch delim {
	case '{':
		obj := newObject()
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, fmt.Errorf("expected an object key, got %v", keyTok)
			}
			value, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			obj.Set(key, value)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return obj, nil

	case '[':
		arr := []any{}
		for dec.More() {
			value, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, value)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}

	return nil, fmt.Errorf("unexpected delimiter %v", delim)
}

// MarshalJSON writes the object with its keys in the recorded order.
//
// The output is compact; the caller re-indents it. json.Indent produces
// the same layout as an Encoder with SetIndent, so the file keeps the
// formatting the rest of this package promises.
func (o *object) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := marshalValue(k)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := marshalValue(o.values[k])
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalValue encodes one value with HTML escaping off, so that an
// ampersand or an angle bracket in a command is not rewritten as &
// and does not produce a spurious diff.
func marshalValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends a newline that the caller must not see.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// decodeObject parses a whole document into an object, rejecting anything
// that is not a single JSON object.
func decodeObject(raw []byte) (*object, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	value, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	// Reject trailing content such as a second document, which decoding
	// only the first value would silently drop on write.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("unexpected data after the JSON object")
	}

	switch v := value.(type) {
	case *object:
		return v, nil
	case nil:
		// The file held a literal "null".
		return newObject(), nil
	default:
		return nil, fmt.Errorf("expected a JSON object, got %T", value)
	}
}
