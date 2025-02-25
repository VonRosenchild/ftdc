// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package bsonx

import (
	"fmt"
	"io"
	"strconv"

	"github.com/mongodb/ftdc/bsonx/bsonerr"
	"github.com/mongodb/ftdc/bsonx/bsontype"
	"github.com/mongodb/ftdc/bsonx/elements"
	"github.com/pkg/errors"
)

const validateMaxDepthDefault = 2048

// Element represents a BSON element, i.e. key-value pair of a BSON document.
//
// NOTE: Element cannot be the value of a map nor a property of a struct without special handling.
// The default encoders and decoders will not process Element correctly. To do so would require
// information loss since an Element contains a key, but the keys used when encoding a struct are
// the struct field names. Instead of using an Element, use a Value as the property of a struct
// field as that is the correct type in this circumstance.
type Element struct {
	value *Value
}

func newElement(start uint32, offset uint32) *Element {
	return &Element{&Value{start: start, offset: offset}}
}

// Clone creates a shallow copy of the element/
func (e *Element) Clone() *Element {
	return &Element{
		value: &Value{
			start:  e.value.start,
			offset: e.value.offset,
			data:   e.value.data,
			d:      e.value.d,
		},
	}
}

// Value returns the value associated with the BSON element.
func (e *Element) Value() *Value {
	return e.value
}

// Validate validates the element and returns its total size.
func (e *Element) Validate() (uint32, error) {
	if e == nil {
		return 0, bsonerr.NilElement
	}
	if e.value == nil {
		return 0, bsonerr.UninitializedElement
	}

	var total uint32 = 1
	n, err := e.validateKey()
	total += n
	if err != nil {
		return total, err
	}
	n, err = e.value.validate(false)
	total += n
	if err != nil {
		return total, err
	}
	return total, nil
}

// validate is a common validation method for elements.
//
// TODO(skriptble): Fill out this method and ensure all validation routines
// pass through this method.
func (e *Element) validate(recursive bool, currentDepth, maxDepth uint32) (uint32, error) {
	return 0, nil
}

func (e *Element) validateKey() (uint32, error) {
	if e.value.data == nil {
		return 0, bsonerr.UninitializedElement
	}

	pos, end := e.value.start+1, e.value.offset
	var total uint32
	if end > uint32(len(e.value.data)) {
		end = uint32(len(e.value.data))
	}
	for ; pos < end && e.value.data[pos] != '\x00'; pos++ {
		total++
	}
	if pos == end || e.value.data[pos] != '\x00' {
		return total, bsonerr.InvalidKey
	}
	total++
	return total, nil
}

// Key returns the key for this element.
// It panics if e is uninitialized.
func (e *Element) Key() string {
	key, ok := e.KeyOK()
	if !ok {
		panic(bsonerr.UninitializedElement)
	}
	return key
}

func (e *Element) KeyOK() (string, bool) {
	if e == nil || e.value == nil || e.value.offset == 0 || e.value.data == nil {
		return "", false
	}

	return string(e.value.data[e.value.start+1 : e.value.offset-1]), true
}

// WriteTo implements the io.WriterTo interface.
func (e *Element) WriteTo(w io.Writer) (int64, error) {
	val, err := e.MarshalBSON()
	if err != nil {
		return 0, errors.WithStack(err)
	}

	n, err := w.Write(val)

	return int64(n), errors.WithStack(err)
}

// WriteElement serializes this element to the provided writer starting at the
// provided start position.
func (e *Element) WriteElement(start uint, writer interface{}) (int64, error) {
	return e.writeElement(true, start, writer)
}

func (e *Element) writeElement(key bool, start uint, writer interface{}) (int64, error) {
	// TODO(skriptble): Figure out if we want to use uint or uint32 and
	// standardize across all packages.
	var total int64
	size, err := e.Validate()
	if err != nil {
		return 0, err
	}
	switch w := writer.(type) {
	case []byte:
		n, err := e.writeByteSlice(key, start, size, w)
		if err != nil {
			return 0, newErrTooSmall()
		}
		total += int64(n)
	case io.Writer:
		return e.WriteTo(w)
	default:
		return 0, bsonerr.InvalidWriter
	}
	return total, nil
}

// writeByteSlice handles writing this element to a slice of bytes.
func (e *Element) writeByteSlice(key bool, start uint, size uint32, b []byte) (int64, error) {
	var startToWrite uint
	needed := start + uint(size)

	if key {
		startToWrite = uint(e.value.start)
	} else {
		startToWrite = uint(e.value.offset)

		// Fewer bytes are needed if the key isn't being written.
		needed -= uint(e.value.offset) - uint(e.value.start) - 1
	}

	if uint(len(b)) < needed {
		return 0, newErrTooSmall()
	}

	var n int
	switch e.value.data[e.value.start] {
	case '\x03':
		if e.value.d == nil {
			n = copy(b[start:], e.value.data[startToWrite:e.value.start+size])
			break
		}

		header := e.value.offset - e.value.start
		size -= header
		if key {
			n += copy(b[start:], e.value.data[startToWrite:e.value.offset])
			start += uint(n)
		}

		nn, err := e.value.d.writeByteSlice(start, size, b)
		n += int(nn)
		if err != nil {
			return int64(n), err
		}
	case '\x04':
		if e.value.d == nil {
			n = copy(b[start:], e.value.data[startToWrite:e.value.start+size])
			break
		}

		header := e.value.offset - e.value.start
		size -= header
		if key {
			n += copy(b[start:], e.value.data[startToWrite:e.value.offset])
			start += uint(n)
		}

		arr := &Array{doc: e.value.d}

		nn, err := arr.writeByteSlice(start, size, b)
		n += int(nn)
		if err != nil {
			return int64(n), err
		}
	case '\x0F':
		// Get length of code
		codeStart := e.value.offset + 4
		codeLength := readi32(e.value.data[codeStart : codeStart+4])

		if e.value.d != nil {
			lengthWithoutScope := 4 + 4 + codeLength

			scopeLength, err := e.value.d.Validate()
			if err != nil {
				return 0, err
			}

			codeWithScopeLength := lengthWithoutScope + int32(scopeLength)
			_, err = elements.Int32.Encode(uint(e.value.offset), e.value.data, codeWithScopeLength)
			if err != nil {
				return int64(n), err
			}

			codeEnd := e.value.offset + uint32(lengthWithoutScope)
			n += copy(
				b[start:],
				e.value.data[startToWrite:codeEnd])
			start += uint(n)

			nn, err := e.value.d.writeByteSlice(start, scopeLength, b)
			n += int(nn)
			if err != nil {
				return int64(n), err
			}

			break
		}

		// Get length of scope
		scopeStart := codeStart + 4 + uint32(codeLength)
		scopeLength := readi32(e.value.data[scopeStart : scopeStart+4])

		// Calculate end of entire CodeWithScope value
		codeWithScopeEnd := int32(scopeStart) + scopeLength

		// Set the length of the value
		codeWithScopeLength := codeWithScopeEnd - int32(e.value.offset)
		_, err := elements.Int32.Encode(uint(e.value.offset), e.value.data, codeWithScopeLength)
		if err != nil {
			return 0, err
		}

		fallthrough
	default:
		n = copy(b[start:], e.value.data[startToWrite:e.value.start+size])
	}

	return int64(n), nil
}

// MarshalBSON implements the Marshaler interface.
func (e *Element) MarshalBSON() ([]byte, error) {
	size, err := e.Validate()
	if err != nil {
		return nil, err
	}
	b := make([]byte, size)
	_, err = e.writeByteSlice(true, 0, size, b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// String implements the fmt.Stringer interface.
func (e *Element) String() string {
	val := e.Value().Interface()
	if s, ok := val.(string); ok && e.Value().Type() == bsontype.String {
		val = strconv.Quote(s)
	}
	return fmt.Sprintf(`bson.Element{[%s]"%s": %v}`, e.Value().Type(), e.Key(), val)
}

// Equal compares this element to element and returns true if they are equal.
func (e *Element) Equal(e2 *Element) bool {
	if e == nil && e2 == nil {
		return true
	}
	if e == nil || e2 == nil {
		return false
	}

	if e.Key() != e2.Key() {
		return false
	}
	return e.value.Equal(e2.value)
}

func elemsFromValues(values []*Value) []*Element {
	elems := make([]*Element, len(values))

	for i, v := range values {
		if v == nil {
			elems[i] = nil
		} else {
			elems[i] = &Element{v}
		}
	}

	return elems
}

func convertValueToElem(key string, v *Value) *Element {
	if v == nil || v.offset == 0 || v.data == nil {
		return nil
	}

	keyLen := len(key)
	// We add the length of the data so when we compare values
	// we don't have extra space at the end of the data property.
	d := make([]byte, 2+len(key)+len(v.data[v.offset:]))

	d[0] = v.data[v.start]
	copy(d[1:keyLen+1], key)
	d[keyLen+1] = 0x00
	copy(d[keyLen+2:], v.data[v.offset:])

	elem := newElement(0, uint32(keyLen+2))
	elem.value.data = d
	elem.value.d = nil
	if v.d != nil {
		elem.value.d = v.d.Copy()
	}

	return elem
}
