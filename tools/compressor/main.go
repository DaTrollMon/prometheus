// Copyright 2013 Prometheus Team
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

package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/lzw"
	"encoding/binary"
	"flag"
	"math"

	"code.google.com/p/snappy-go/snappy"

	"github.com/golang/glog"

	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/storage/metric"
	"github.com/prometheus/prometheus/storage/metric/tiered"
)

var (
	storageRoot   = flag.String("storage.root", "", "The path to the storage root for Prometheus.")
	dieOnBadChunk = flag.Bool("dieOnBadChunk", false, "Whether to die upon encountering a bad chunk.")
)

const sampleSize = 16

type compressFn func([]byte) int

type SamplesCompressor struct {
	dest              []byte
	chunks            int
	samples           int
	uncompressedBytes int
	compressors       map[string]compressFn
	compressedBytes   map[string]int
}

func (c *SamplesCompressor) Operate(key, value interface{}) *storage.OperatorError {
	v := value.(metric.Values)

	glog.Info("Chunk size: ", len(v))
	c.chunks++
	c.samples += len(v)

	sz := len(v) * sampleSize
	if cap(c.dest) < sz {
		c.dest = make([]byte, sz)
	} else {
		c.dest = c.dest[0:sz]
	}
	for i, val := range v {
		offset := i * sampleSize
		binary.LittleEndian.PutUint64(c.dest[offset:], uint64(val.Timestamp.Unix()))
		binary.LittleEndian.PutUint64(c.dest[offset+8:], math.Float64bits(float64(val.Value)))
	}
	c.uncompressedBytes += sz

	for algo, fn := range c.compressors {
		c.compressedBytes[algo] += fn(c.dest)
	}
	return nil
}

func (c *SamplesCompressor) Report() {
	glog.Infof("Chunks: %d", c.chunks)
	glog.Infof("Samples: %d", c.samples)
	glog.Infof("Avg. chunk size: %d", c.samples/c.chunks)
	glog.Infof("Total: %d (100%%)", c.uncompressedBytes)
	for algo, _ := range c.compressors {
		glog.Infof("%s: %d (%.1f%%)", algo, c.compressedBytes[algo], 100*float64(c.compressedBytes[algo])/float64(c.uncompressedBytes))
	}
}

var compressors = map[string]compressFn{
	"gzip": func(v []byte) int {
		var b bytes.Buffer
		w, err := gzip.NewWriterLevel(&b, gzip.BestCompression)
		if err != nil {
			glog.Fatal(err)
		}
		w.Write(v)
		w.Close()
		return b.Len()
	},
	"flate": func(v []byte) int {
		var b bytes.Buffer
		w, err := flate.NewWriter(&b, flate.BestCompression)
		if err != nil {
			glog.Fatal(err)
		}
		w.Write(v)
		w.Close()
		return b.Len()
	},
	"lzw": func(v []byte) int {
		var b bytes.Buffer
		w := lzw.NewWriter(&b, lzw.MSB, 8)
		w.Write(v)
		w.Close()
		return b.Len()
	},
	"snappy": func(v []byte) int {
		c, err := snappy.Encode(nil, v)
		if err != nil {
			glog.Fatal(err)
		}
		return len(c)
	},
}

func main() {
	flag.Parse()

	if storageRoot == nil || *storageRoot == "" {
		glog.Fatal("Must provide a path...")
	}

	persistence, err := tiered.NewLevelDBPersistence(*storageRoot)
	if err != nil {
		glog.Fatal(err)
	}
	defer persistence.Close()

	c := &SamplesCompressor{
		compressors:     compressors,
		compressedBytes: make(map[string]int),
	}

	entire, err := persistence.MetricSamples.ForEach(&tiered.MetricSamplesDecoder{}, &tiered.AcceptAllFilter{}, c)
	if err != nil {
		glog.Fatal("Error compressing samples: ", err)
	}
	if !entire {
		glog.Fatal("Didn't scan entire corpus")
	}
	c.Report()
}
