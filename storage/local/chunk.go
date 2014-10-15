// Copyright 2014 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package local

import (
	"io"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/storage/metric"
)

// chunk is the interface for all chunks. Chunks are generally not
// goroutine-safe.
type chunk interface {
	// add adds a SamplePair to the chunks, performs any necessary
	// re-encoding, and adds any necessary overflow chunks. It returns the
	// new version of the original chunk, followed by overflow chunks, if
	// any. The first chunk returned might be the same as the original one
	// or a newly allocated version. In any case, take the returned chunk as
	// the relevant one and discard the orginal chunk.
	add(*metric.SamplePair) []chunk
	clone() chunk
	firstTime() clientmodel.Timestamp
	lastTime() clientmodel.Timestamp
	newIterator() chunkIterator
	marshal(io.Writer) error
	unmarshal(io.Reader) error
	// values returns a channel, from which all sample values in the chunk
	// can be received in order. The channel is closed after the last
	// one. It is generally not safe to mutate the chunk while the channel
	// is still open.
	values() <-chan *metric.SamplePair
}

// A chunkIterator enables efficient access to the content of a chunk. It is
// generally not safe to use a chunkIterator concurrently with or after chunk
// mutation.
type chunkIterator interface {
	// Gets the two values that are immediately adjacent to a given time. In
	// case a value exist at precisely the given time, only that single
	// value is returned. Only the first or last value is returned (as a
	// single value), if the given time is before or after the first or last
	// value, respectively.
	getValueAtTime(clientmodel.Timestamp) metric.Values
	// Gets all values contained within a given interval.
	getRangeValues(metric.Interval) metric.Values
	// Whether a given timestamp is contained between first and last value
	// in the chunk.
	contains(clientmodel.Timestamp) bool
}

func transcodeAndAdd(dst chunk, src chunk, s *metric.SamplePair) []chunk {
	numTranscodes.Inc()

	head := dst
	body := []chunk{}
	for v := range src.values() {
		newChunks := head.add(v)
		body = append(body, newChunks[:len(newChunks)-1]...)
		head = newChunks[len(newChunks)-1]
	}
	newChunks := head.add(s)
	body = append(body, newChunks[:len(newChunks)-1]...)
	head = newChunks[len(newChunks)-1]
	return append(body, head)
}

func chunkType(c chunk) byte {
	switch c.(type) {
	case *deltaEncodedChunk:
		return 0
	default:
		panic("unknown chunk type")
	}
}

func chunkForType(chunkType byte) chunk {
	switch chunkType {
	case 0:
		return newDeltaEncodedChunk(d1, d0, true)
	default:
		panic("unknown chunk type")
	}
}
