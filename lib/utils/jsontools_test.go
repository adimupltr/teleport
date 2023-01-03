/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package utils

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/internalutils/stream"
	"github.com/gravitational/teleport/api/types"
)

// TestMarshalMapConsistency ensures serialized byte comparisons succeed
// after multiple serialize/deserialize round trips. Some JSON marshaling
// backends don't sort map keys for performance reasons, which can make
// operations that depend on the byte ordering fail (e.g. CompareAndSwap).
func TestMarshalMapConsistency(t *testing.T) {
	t.Parallel()

	value := map[string]string{
		types.TeleportNamespace + "/foo": "1234",
		types.TeleportNamespace + "/bar": "5678",
	}

	compareTo, err := FastMarshal(value)
	require.NoError(t, err)

	for i := 0; i < 100; i++ {
		roundTrip := make(map[string]string)
		err := FastUnmarshal(compareTo, &roundTrip)
		require.NoError(t, err)

		val, err := FastMarshal(roundTrip)
		require.NoError(t, err)

		require.Truef(t, bytes.Equal(val, compareTo), "maps must serialize consistently (attempt %d)", i)
	}
}

type testObj struct {
	S string `json:"s"`
	N int    `json:"n"`
}

func TestStreamJSONArray(t *testing.T) {
	objects := []testObj{
		{"spam", 1},
		{"eggs", 2},
		{"zero", 0},
		{"five", 5},
		{"", 100},
		{},
		{"neg", -100},
	}

	var objBuf bytes.Buffer
	err := StreamJSONArray(stream.Slice(objects), &objBuf, true)
	require.NoError(t, err)

	var objOut []testObj
	err = FastUnmarshal(objBuf.Bytes(), &objOut)
	require.NoError(t, err)
	require.Equal(t, objects, objOut)

	numbers := []int{
		0,
		-1,
		12,
		15,
		1000,
		0,
		1,
	}

	var numBuf bytes.Buffer
	err = StreamJSONArray(stream.Slice(numbers), &numBuf, false)
	require.NoError(t, err)

	var numOut []int
	err = FastUnmarshal(numBuf.Bytes(), &numOut)
	require.NoError(t, err)
	require.Equal(t, numbers, numOut)

	var iterative []string
	for i := 0; i < 100; i++ {
		var iterBuf bytes.Buffer
		err = StreamJSONArray(stream.Slice(iterative), &iterBuf, false)
		require.NoError(t, err)

		var iterOut []string
		err = FastUnmarshal(iterBuf.Bytes(), &iterOut)
		require.NoError(t, err)

		if len(iterative) == 0 {
			require.Len(t, iterOut, 0)
		} else {
			require.Equal(t, iterative, iterOut)
		}

		// we add at the end of the loop so that this test case
		// covers the empty slice case.
		iterative = append(iterative, fmt.Sprintf("%d", i))
	}
}
