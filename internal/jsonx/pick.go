// Package jsonx picks values out of JSON documents whose exact shape is not
// documented.
//
// The private endpoints behind Action and Allegro are not published, and their
// field names have changed before. Decoding them into a fixed struct would turn
// every rename into a silent field of zeroes; the helpers here look for a value
// under any of several plausible keys, at any depth, and say when they found
// nothing.
package jsonx

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Object is a decoded JSON object.
type Object map[string]any

// Decode unmarshals a JSON object.
func Decode(data []byte) (Object, error) {
	var obj Object
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// Get returns the first value found under any of the given keys, comparing key
// names case-insensitively. Only the top level of the object is searched.
func (o Object) Get(keys ...string) (any, bool) {
	for _, key := range keys {
		for actual, value := range o {
			if strings.EqualFold(actual, key) {
				return value, true
			}
		}
	}
	return nil, false
}

// String returns the first key that holds a string or a number.
func (o Object) String(keys ...string) string {
	value, ok := o.Get(keys...)
	if !ok {
		return ""
	}
	return AsString(value)
}

// Object returns the first key that holds a nested object.
func (o Object) Object(keys ...string) Object {
	value, ok := o.Get(keys...)
	if !ok {
		return nil
	}
	nested, _ := value.(map[string]any)
	return Object(nested)
}

// Array returns the first key that holds an array. The second result says
// whether such a key was there at all, which is how an empty list ("you bought
// nothing this month") stays distinguishable from a payload whose shape moved.
func (o Object) Array(keys ...string) ([]Object, bool) {
	value, ok := o.Get(keys...)
	if !ok {
		return nil, false
	}
	if _, isArray := value.([]any); !isArray {
		return nil, false
	}
	return AsObjects(value), true
}

// Float returns the first key that holds a number, or a string that parses as
// one. The second result says whether a value was found.
func (o Object) Float(keys ...string) (float64, bool) {
	value, ok := o.Get(keys...)
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return typed, true
	case string:
		parsed, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(typed), ",", "."), 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	}
	return 0, false
}

// Bool returns the first key that holds a boolean.
func (o Object) Bool(keys ...string) bool {
	value, ok := o.Get(keys...)
	if !ok {
		return false
	}
	b, _ := value.(bool)
	return b
}

// FindArray walks the whole document, breadth first, for the first array
// stored under any of the given keys. Private APIs like to wrap their payload
// in one or two envelope objects, and the wrapper is what changes. The second
// result says whether the key was found; an empty array found at the top level
// wins over a deeper non-empty one, because that is the caller's own list.
func (o Object) FindArray(keys ...string) ([]Object, bool) {
	if found, ok := o.Array(keys...); ok {
		return found, true
	}
	queue := []Object{o}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, value := range current {
			switch typed := value.(type) {
			case map[string]any:
				nested := Object(typed)
				if found, ok := nested.Array(keys...); ok {
					return found, true
				}
				queue = append(queue, nested)
			case []any:
				for _, element := range typed {
					if nested, ok := element.(map[string]any); ok {
						queue = append(queue, Object(nested))
					}
				}
			}
		}
	}
	return nil, false
}

// AsString renders a JSON scalar as a string. Numbers keep full precision so
// that "12.30" does not become "12.3" before it reaches the money parser.
func AsString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case json.Number:
		return typed.String()
	}
	return ""
}

// AsObjects converts a JSON array into objects, skipping other elements.
func AsObjects(value any) []Object {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]Object, 0, len(list))
	for _, element := range list {
		if nested, ok := element.(map[string]any); ok {
			out = append(out, Object(nested))
		}
	}
	return out
}

// timeLayouts are the formats these APIs and their e-mails have been seen to use.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05.999Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"02.01.2006 15:04",
	"02.01.2006",
	"02-01-2006",
	"02/01/2006",
}

// ParseTime reads a timestamp in any of the layouts these sources use. A
// numeric value is treated as Unix seconds (or milliseconds when large).
func ParseTime(value any) time.Time {
	switch typed := value.(type) {
	case float64:
		if typed > 1e12 {
			return time.UnixMilli(int64(typed)).UTC()
		}
		if typed > 0 {
			return time.Unix(int64(typed), 0).UTC()
		}
		return time.Time{}
	case string:
		return ParseTimeString(typed)
	}
	return time.Time{}
}

// ParseTimeString reads a timestamp string in any known layout.
func ParseTimeString(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Time returns the first key that holds a parsable timestamp.
func (o Object) Time(keys ...string) time.Time {
	value, ok := o.Get(keys...)
	if !ok {
		return time.Time{}
	}
	return ParseTime(value)
}
