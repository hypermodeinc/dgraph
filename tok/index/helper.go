/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package index

import (
	"encoding/binary"
	"math"
	"reflect"
	"unsafe"

	"github.com/golang/glog"
	c "github.com/hypermodeinc/dgraph/v25/tok/constraints"
)

// BytesAsFloatArray[T c.Float](encoded) converts encoded into a []T,
// where T is either float32 or float64, depending on the value of floatBits.
// Let floatBytes = floatBits/8. If len(encoded) % floatBytes is
// not 0, it will ignore any trailing bytes, and simply convert floatBytes
// bytes at a time to generate the entries.
// The result is appended to the given retVal slice. If retVal is nil
// then a new slice is created and appended to.
func BytesAsFloatArray[T c.Float](encoded []byte, retVal *[]T, floatBits int) {
	floatBytes := floatBits / 8

	if len(encoded) == 0 {
		*retVal = []T{}
		return
	}

	// Ensure the byte slice length is a multiple of 8 (size of float64)
	if len(encoded)%floatBytes != 0 {
		glog.Errorf("Invalid byte slice length %d %v", len(encoded), encoded)
		return
	}

	if retVal == nil {
		*retVal = make([]T, len(encoded)/floatBytes)
	}
	*retVal = (*retVal)[:0]
	header := (*reflect.SliceHeader)(unsafe.Pointer(retVal))
	header.Data = uintptr(unsafe.Pointer(&encoded[0]))
	header.Len = len(encoded) / floatBytes
	header.Cap = len(encoded) / floatBytes
}

func BytesToFloat[T c.Float](encoded []byte, floatBits int) T {
	if floatBits == 32 {
		bits := binary.LittleEndian.Uint32(encoded)
		return T(math.Float32frombits(bits))
	} else if floatBits == 64 {
		bits := binary.LittleEndian.Uint64(encoded)
		return T(math.Float64frombits(bits))
	}
	panic("Invalid floatBits")
}
