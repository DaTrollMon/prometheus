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
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/storage/local/codable"
	"github.com/prometheus/prometheus/storage/local/index"
	"github.com/prometheus/prometheus/storage/metric"
)

const (
	seriesFileSuffix     = ".db"
	seriesTempFileSuffix = ".db.tmp"

	headsFileName      = "heads.db"
	headsTempFileName  = "heads.db.tmp"
	headsFormatVersion = 1
	headsMagicString   = "PrometheusHeads"

	fileBufSize = 1 << 16 // 64kiB.

	chunkHeaderLen             = 17
	chunkHeaderTypeOffset      = 0
	chunkHeaderFirstTimeOffset = 1
	chunkHeaderLastTimeOffset  = 9

	indexingMaxBatchSize  = 1024 * 1024
	indexingBatchTimeout  = 500 * time.Millisecond // Commit batch when idle for that long.
	indexingQueueCapacity = 1024
)

const (
	flagHeadChunkPersisted byte = 1 << iota
	// Add more flags here like:
	// flagFoo
	// flagBar
)

type indexingOpType byte

const (
	add indexingOpType = iota
	remove
)

type indexingOp struct {
	fingerprint clientmodel.Fingerprint
	metric      clientmodel.Metric
	opType      indexingOpType
}

// A Persistence is used by a Storage implementation to store samples
// persistently across restarts. The methods are only goroutine-safe if
// explicitly marked as such below. The chunk-related methods PersistChunk,
// DropChunks, LoadChunks, and LoadChunkDescs can be called concurrently with
// each other if each call refers to a different fingerprint.
type persistence struct {
	basePath string
	chunkLen int

	// archiveMtx protects the archiving-related methods archiveMetric,
	// unarchiveMetric, dropArchiveMetric, and getFingerprintsModifiedBefore
	// from concurrent calls.
	archiveMtx sync.Mutex

	archivedFingerprintToMetrics   *index.FingerprintMetricIndex
	archivedFingerprintToTimeRange *index.FingerprintTimeRangeIndex
	labelPairToFingerprints        *index.LabelPairFingerprintIndex
	labelNameToLabelValues         *index.LabelNameLabelValuesIndex

	indexingQueue   chan indexingOp
	indexingStopped chan struct{}
	indexingFlush   chan chan int

	indexingQueueLength   prometheus.Gauge
	indexingQueueCapacity prometheus.Metric
	indexingBatchSizes    prometheus.Summary
	indexingBatchLatency  prometheus.Summary
	checkpointDuration    prometheus.Gauge

	dirtyMtx sync.Mutex // Protects dirty and becameDirty.

	dirty, becameDirty bool
}

// newPersistence returns a newly allocated persistence backed by local disk storage, ready to use.
func newPersistence(basePath string, chunkLen int, dirty bool) (*persistence, error) {
	if err := os.MkdirAll(basePath, 0700); err != nil {
		return nil, err
	}
	var err error
	archivedFingerprintToMetrics, err := index.NewFingerprintMetricIndex(basePath)
	if err != nil {
		return nil, err
	}
	archivedFingerprintToTimeRange, err := index.NewFingerprintTimeRangeIndex(basePath)
	if err != nil {
		return nil, err
	}
	labelPairToFingerprints, err := index.NewLabelPairFingerprintIndex(basePath)
	if err != nil {
		return nil, err
	}
	labelNameToLabelValues, err := index.NewLabelNameLabelValuesIndex(basePath)
	if err != nil {
		return nil, err
	}

	p := &persistence{
		basePath: basePath,
		chunkLen: chunkLen,

		archivedFingerprintToMetrics:   archivedFingerprintToMetrics,
		archivedFingerprintToTimeRange: archivedFingerprintToTimeRange,
		labelPairToFingerprints:        labelPairToFingerprints,
		labelNameToLabelValues:         labelNameToLabelValues,

		indexingQueue:   make(chan indexingOp, indexingQueueCapacity),
		indexingStopped: make(chan struct{}),
		indexingFlush:   make(chan chan int),

		indexingQueueLength: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "indexing_queue_length",
			Help:      "The number of metrics waiting to be indexed.",
		}),
		indexingQueueCapacity: prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, subsystem, "indexing_queue_capacity"),
				"The capacity of the indexing queue.",
				nil, nil,
			),
			prometheus.GaugeValue,
			float64(indexingQueueCapacity),
		),
		indexingBatchSizes: prometheus.NewSummary(
			prometheus.SummaryOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "indexing_batch_sizes",
				Help:      "Quantiles for indexing batch sizes (number of metrics per batch).",
			},
		),
		indexingBatchLatency: prometheus.NewSummary(
			prometheus.SummaryOpts{
				Namespace: namespace,
				Subsystem: subsystem,
				Name:      "indexing_batch_latency_milliseconds",
				Help:      "Quantiles for batch indexing latencies in milliseconds.",
			},
		),
		checkpointDuration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "checkpoint_duration_milliseconds",
			Help:      "The duration (in milliseconds) it took to checkpoint in-memory metrics and head chunks.",
		}),

		dirty: dirty,
	}

	if dirtyFile, err := os.OpenFile(p.dirtyFileName(), os.O_CREATE|os.O_EXCL, 0666); err == nil {
		dirtyFile.Close()
	} else if os.IsExist(err) {
		p.dirty = true
	} else {
		return nil, err
	}

	go p.processIndexingQueue()
	return p, nil
}

// Describe implements prometheus.Collector.
func (p *persistence) Describe(ch chan<- *prometheus.Desc) {
	ch <- p.indexingQueueLength.Desc()
	ch <- p.indexingQueueCapacity.Desc()
	p.indexingBatchSizes.Describe(ch)
	p.indexingBatchLatency.Describe(ch)
	ch <- p.checkpointDuration.Desc()
}

// Collect implements prometheus.Collector.
func (p *persistence) Collect(ch chan<- prometheus.Metric) {
	p.indexingQueueLength.Set(float64(len(p.indexingQueue)))

	ch <- p.indexingQueueLength
	ch <- p.indexingQueueCapacity
	p.indexingBatchSizes.Collect(ch)
	p.indexingBatchLatency.Collect(ch)
	ch <- p.checkpointDuration
}

// dirtyFileName returns the name of the (empty) file used to mark the
// persistency layer as dirty.
func (p *persistence) dirtyFileName() string {
	return path.Join(p.basePath, "DIRTY")
}

// isDirty returns the dirty flag in a goroutine-safe way.
func (p *persistence) isDirty() bool {
	p.dirtyMtx.Lock()
	defer p.dirtyMtx.Unlock()
	return p.dirty
}

// setDirty sets the dirty flag in a goroutine-safe way. Once the dirty flag was
// set to true with this method, it cannot be set to false again. (If we became
// dirty during our runtime, there is no way back. If we were dirty from the
// start, a clean-up might make us clean again.)
func (p *persistence) setDirty(dirty bool) {
	p.dirtyMtx.Lock()
	defer p.dirtyMtx.Unlock()
	if p.becameDirty {
		return
	}
	p.dirty = dirty
	if dirty {
		p.becameDirty = true
	}
}

// cleanUpStage1 is called by loadSeriesMapAndHeads if the persistence appears
// to be dirty after the loading (either because the loading resulted in an
// error or because the persistence was dirty from the start).
func (p *persistence) cleanUpStage1(fingerprintToSeries map[clientmodel.Fingerprint]*memorySeries) error {
	glog.Warning("Starting clean-up stage 1. Prometheus is inoperational until stage 1 is complete.")

	fpsSeen := map[clientmodel.Fingerprint]struct{}{}

	for i := 0; i < 256; i++ {
		dirname := path.Join(p.basePath, fmt.Sprintf("%02x", i))
		dir, err := os.Open(dirname)
		if err == os.ErrNotExist {
			continue
		}
		if err != nil {
			return err
		}
		defer dir.Close()
		var fis []os.FileInfo
		for ; err != io.EOF; fis, err = dir.Readdir(1024) {
			if err != nil {
				return err
			}
			for _, fi := range fis {
				fp, ok := p.sanitizeSeries(dirname, fi, fingerprintToSeries)
				if ok {
					fpsSeen[fp] = struct{}{}
				}
			}
		}
	}

	for fp, s := range fingerprintToSeries {
		if _, seen := fpsSeen[fp]; !seen {
			// fp exists in fingerprintToSeries, but has no representation on disk.
			if s.headChunkPersisted {
				// Oops, head chunk was persisted, but nothing on disk.
				// Thus, we lost that series completely. Clean up the remnants.
				delete(fingerprintToSeries, fp)
				if err := p.dropArchivedMetric(fp); err != nil {
					// Dropping the archived metric didn't work, so try
					// to unindex it, just in case it's in the indexes.
					p.unindexMetric(fp, s.metric)
				}
				glog.Warningf("Lost series detected: fingerprint %v, metric %v.", fp, s.metric)
				continue
			}
			// If we are here, the only chunk we have is the head chunk.
			// Adjust things accordingly.
			if len(s.chunkDescs) > 1 || s.chunkDescsOffset != 0 {
				glog.Warningf(
					"Lost at least %d chunks for fingerprint %v, metric %v.",
					len(s.chunkDescs)+s.chunkDescsOffset-1, fp, s.metric,
					// If chunkDescsOffset is -1, this will underreport. Oh well...
				)
				s.chunkDescs = s.chunkDescs[len(s.chunkDescs)-1:]
				s.chunkDescsOffset = 0
			}
		}
	}

	glog.Warning("Clean-up stage 1 completed.")
	return nil
}

// TODO: Document.
func (p *persistence) sanitizeSeries(dirname string, fi os.FileInfo, fingerprintToSeries map[clientmodel.Fingerprint]*memorySeries) (clientmodel.Fingerprint, bool) {
	filename := path.Join(dirname, fi.Name())
	purge := func() {
		glog.Warningf("Deleting lost series file %s.", filename) // TODO: Move to lost+found directory?
		os.Remove(filename)
	}

	var fp clientmodel.Fingerprint
	if len(fi.Name()) != 17 || !strings.HasSuffix(fi.Name(), ".db") {
		glog.Warningf("Unexpected series file name %s.", filename)
		purge()
		return fp, false
	}
	fp.LoadFromString(path.Base(dirname) + fi.Name()) // TODO: Panics if fi.Name() doesn't parse as hex.

	bytesToTrim := fi.Size() % int64(p.chunkLen+chunkHeaderLen)
	chunksInFile := int(fi.Size())/p.chunkLen + chunkHeaderLen
	if bytesToTrim != 0 {
		glog.Warningf(
			"Truncating file %s to exactly %d chunks, trimming %d extraneous bytes.",
			filename, chunksInFile, bytesToTrim,
		)
		f, err := os.OpenFile(filename, os.O_WRONLY, 0640)
		if err != nil {
			glog.Errorf("Could not open file %s: %s", filename, err)
			return fp, true
		}
		if err := f.Truncate(fi.Size() - bytesToTrim); err != nil {
			glog.Errorf("Failed to truncate file %s: %s", filename, err)
			return fp, true
		}
	}

	s, ok := fingerprintToSeries[fp]
	if ok {
		// This series is supposed to not be archived.
		if s == nil {
			panic("fingerprint mapped to nil pointer")
		}
		if bytesToTrim == 0 && s.chunkDescsOffset != -1 &&
			(s.headChunkPersisted && chunksInFile == s.chunkDescsOffset+len(s.chunkDescs)) ||
			(!s.headChunkPersisted && chunksInFile == s.chunkDescsOffset+len(s.chunkDescs)-1) {
			// Everything is consistent. We are good.
			return fp, true
		}
		// TODO deal with remaining cases.
	} else {
		// This series is supposed to be archived.
		metric, err := p.getArchivedMetric(fp)
		if err != nil {
			glog.Errorf(
				"Fingerprint %v assumed archived but couldn't be looked up in archived index: %s",
				fp, err,
			)
			purge()
			return fp, true
		}
		if metric == nil {
			glog.Warningf(
				"Fingerprint %v assumed archived but couldn't be found in archived index.",
				fp,
			)
			purge()
			return fp, true
		}
		// This series looks like a properly archived one.
	}
	return fp, true
}

// cleanUpStage2 completes the clean-up of a dirty persistence. It is run as
// a goroutine in parallel to normal operations (but queries might yield
// incomplete results). If it is successful, it will unset the dirty flag of
// the persistence.
func (p *persistence) cleanUpStage2(fingerprintToSeries *seriesMap) {
	glog.Warning("Starting clean-up stage 2. Prometheus is able to serve now, but queries may yield incomplete results.")
	// TODO: Implement.
	p.setDirty(false)
	glog.Warning("Clean-up stage 2 completed. Prometheus is fully operational.")
}

// getFingerprintsForLabelPair returns the fingerprints for the given label
// pair. This method is goroutine-safe but take into account that metrics queued
// for indexing with IndexMetric might not yet made it into the index. (Same
// applies correspondingly to UnindexMetric.)
func (p *persistence) getFingerprintsForLabelPair(lp metric.LabelPair) (clientmodel.Fingerprints, error) {
	fps, _, err := p.labelPairToFingerprints.Lookup(lp)
	if err != nil {
		return nil, err
	}
	return fps, nil
}

// getLabelValuesForLabelName returns the label values for the given label
// name. This method is goroutine-safe but take into account that metrics queued
// for indexing with IndexMetric might not yet made it into the index. (Same
// applies correspondingly to UnindexMetric.)
func (p *persistence) getLabelValuesForLabelName(ln clientmodel.LabelName) (clientmodel.LabelValues, error) {
	lvs, _, err := p.labelNameToLabelValues.Lookup(ln)
	if err != nil {
		return nil, err
	}
	return lvs, nil
}

// persistChunk persists a single chunk of a series. It is the caller's
// responsibility to not modify chunk concurrently and to not persist or drop
// anything for the same fingerprint concurrently. It returns the (zero-based)
// index of the persisted chunk within the series file. In case of an error, the
// returned index is -1 (to avoid the misconception that the chunk was written
// at position 0).
func (p *persistence) persistChunk(fp clientmodel.Fingerprint, c chunk) (int, error) {
	// 1. Open chunk file.
	f, err := p.openChunkFileForWriting(fp)
	if err != nil {
		return -1, err
	}
	defer f.Close()

	b := bufio.NewWriterSize(f, chunkHeaderLen+p.chunkLen)

	// 2. Write the header (chunk type and first/last times).
	err = writeChunkHeader(b, c)
	if err != nil {
		return -1, err
	}

	// 3. Write chunk into file.
	err = c.marshal(b)
	if err != nil {
		return -1, err
	}

	// 4. Determine index within the file.
	b.Flush()
	offset, err := f.Seek(0, os.SEEK_CUR)
	if err != nil {
		return -1, err
	}
	index, err := p.chunkIndexForOffset(offset)
	if err != nil {
		return -1, err
	}

	return index - 1, err
}

// loadChunks loads a group of chunks of a timeseries by their index. The chunk
// with the earliest time will have index 0, the following ones will have
// incrementally larger indexes. The indexOffset denotes the offset to be added to
// each index in indexes. It is the caller's responsibility to not persist or
// drop anything for the same fingerprint concurrently.
func (p *persistence) loadChunks(fp clientmodel.Fingerprint, indexes []int, indexOffset int) ([]chunk, error) {
	// TODO: we need to verify at some point that file length is a multiple of
	// the chunk size. When is the best time to do this, and where to remember
	// it? Right now, we only do it when loading chunkDescs.
	f, err := p.openChunkFileForReading(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	chunks := make([]chunk, 0, len(indexes))
	typeBuf := make([]byte, 1)
	for _, idx := range indexes {
		_, err := f.Seek(p.offsetForChunkIndex(idx+indexOffset), os.SEEK_SET)
		if err != nil {
			return nil, err
		}

		n, err := f.Read(typeBuf)
		if err != nil {
			return nil, err
		}
		if n != 1 {
			panic("read returned != 1 bytes")
		}

		_, err = f.Seek(chunkHeaderLen-1, os.SEEK_CUR)
		if err != nil {
			return nil, err
		}
		chunk := chunkForType(typeBuf[0])
		chunk.unmarshal(f)
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

// loadChunkDescs loads chunkDescs for a series up until a given time.  It is
// the caller's responsibility to not persist or drop anything for the same
// fingerprint concurrently.
func (p *persistence) loadChunkDescs(fp clientmodel.Fingerprint, beforeTime clientmodel.Timestamp) ([]*chunkDesc, error) {
	f, err := p.openChunkFileForReading(fp)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	totalChunkLen := chunkHeaderLen + p.chunkLen
	if fi.Size()%int64(totalChunkLen) != 0 {
		// TODO: record number of encountered corrupt series files in a metric?

		// Truncate the file size to the nearest multiple of chunkLen.
		truncateTo := fi.Size() - fi.Size()%int64(totalChunkLen)
		glog.Infof("Bad series file size for %s: %d bytes (no multiple of %d). Truncating to %d bytes.", fp, fi.Size(), totalChunkLen, truncateTo)
		// TODO: this doesn't work, as this is a read-only file handle.
		if err := f.Truncate(truncateTo); err != nil {
			return nil, err
		}
	}

	numChunks := int(fi.Size()) / totalChunkLen
	cds := make([]*chunkDesc, 0, numChunks)
	for i := 0; i < numChunks; i++ {
		_, err := f.Seek(p.offsetForChunkIndex(i)+chunkHeaderFirstTimeOffset, os.SEEK_SET)
		if err != nil {
			return nil, err
		}

		chunkTimesBuf := make([]byte, 16)
		_, err = io.ReadAtLeast(f, chunkTimesBuf, 16)
		if err != nil {
			return nil, err
		}
		cd := &chunkDesc{
			chunkFirstTime: clientmodel.Timestamp(binary.LittleEndian.Uint64(chunkTimesBuf)),
			chunkLastTime:  clientmodel.Timestamp(binary.LittleEndian.Uint64(chunkTimesBuf[8:])),
		}
		if !cd.chunkLastTime.Before(beforeTime) {
			// From here on, we have chunkDescs in memory already.
			break
		}
		cds = append(cds, cd)
	}
	chunkDescOps.WithLabelValues(load).Add(float64(len(cds)))
	atomic.AddInt64(&numMemChunkDescs, int64(len(cds)))
	return cds, nil
}

// checkpointSeriesMapAndHeads persists the fingerprint to memory-series mapping
// and all open (non-full) head chunks. Do not call concurrently with
// loadSeriesMapAndHeads.
func (p *persistence) checkpointSeriesMapAndHeads(fingerprintToSeries *seriesMap, fpLocker *fingerprintLocker) (err error) {
	glog.Info("Checkpointing in-memory metrics and head chunks...")
	begin := time.Now()
	f, err := os.OpenFile(p.headsTempFileName(), os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0640)
	if err != nil {
		return
	}

	defer func() {
		closeErr := f.Close()
		if err != nil {
			return
		}
		err = closeErr
		if err != nil {
			return
		}
		err = os.Rename(p.headsTempFileName(), p.headsFileName())
		duration := time.Since(begin)
		p.checkpointDuration.Set(float64(duration) / float64(time.Millisecond))
		glog.Infof("Done checkpointing in-memory metrics and head chunks in %v.", duration)
	}()

	w := bufio.NewWriterSize(f, fileBufSize)

	if _, err = w.WriteString(headsMagicString); err != nil {
		return
	}
	var numberOfSeriesOffset int
	if numberOfSeriesOffset, err = codable.EncodeVarint(w, headsFormatVersion); err != nil {
		return
	}
	numberOfSeriesOffset += len(headsMagicString)
	numberOfSeriesInHeader := uint64(fingerprintToSeries.length())
	// We have to write the number of series as uint64 because we might need
	// to overwrite it later, and a varint might change byte width then.
	if err = codable.EncodeUint64(w, numberOfSeriesInHeader); err != nil {
		return
	}

	iter := fingerprintToSeries.iter()
	defer func() {
		// Consume the iterator in any case to not leak goroutines.
		for _ = range iter {
		}
	}()

	var realNumberOfSeries uint64
	for m := range iter {
		func() { // Wrapped in function to use defer for unlocking the fp.
			fpLocker.Lock(m.fp)
			defer fpLocker.Unlock(m.fp)

			if len(m.series.chunkDescs) == 0 {
				// This series was completely purged or archived in the meantime. Ignore.
				return
			}
			realNumberOfSeries++
			var seriesFlags byte
			if m.series.headChunkPersisted {
				seriesFlags |= flagHeadChunkPersisted
			}
			if err = w.WriteByte(seriesFlags); err != nil {
				return
			}
			if err = codable.EncodeUint64(w, uint64(m.fp)); err != nil {
				return
			}
			var buf []byte
			buf, err = codable.Metric(m.series.metric).MarshalBinary()
			if err != nil {
				return
			}
			w.Write(buf)
			if _, err = codable.EncodeVarint(w, int64(m.series.chunkDescsOffset)); err != nil {
				return
			}
			if _, err = codable.EncodeVarint(w, int64(len(m.series.chunkDescs))); err != nil {
				return
			}
			for i, chunkDesc := range m.series.chunkDescs {
				if m.series.headChunkPersisted || i < len(m.series.chunkDescs)-1 {
					if _, err = codable.EncodeVarint(w, int64(chunkDesc.firstTime())); err != nil {
						return
					}
					if _, err = codable.EncodeVarint(w, int64(chunkDesc.lastTime())); err != nil {
						return
					}
				} else {
					// This is the non-persisted head chunk. Fully marshal it.
					if err = w.WriteByte(chunkType(chunkDesc.chunk)); err != nil {
						return
					}
					if err = chunkDesc.chunk.marshal(w); err != nil {
						return
					}
				}
			}
		}()
		if err != nil {
			return
		}
	}
	if err = w.Flush(); err != nil {
		return
	}
	if realNumberOfSeries != numberOfSeriesInHeader {
		// The number of series has changed in the meantime.
		// Rewrite it in the header.
		if _, err = f.Seek(int64(numberOfSeriesOffset), os.SEEK_SET); err != nil {
			return
		}
		if err = codable.EncodeUint64(f, realNumberOfSeries); err != nil {
			return
		}
	}
	return
}

// loadSeriesMapAndHeads loads the fingerprint to memory-series mapping and all
// open (non-full) head chunks. If recoverable corruption is detected, or if the
// dirty flag was set from the beginning, stage 1 of the clean-up is run. The
// method will return once it is done. An unrecoverable error is returned. Call
// this method during start-up while nothing else is running in storage
// land. This method is utterly goroutine-unsafe.
func (p *persistence) loadSeriesMapAndHeads() (sm *seriesMap, err error) {
	var chunksTotal, chunkDescsTotal int64
	fingerprintToSeries := make(map[clientmodel.Fingerprint]*memorySeries)
	sm = &seriesMap{m: fingerprintToSeries}

	defer func() {
		if sm != nil && p.dirty {
			glog.Warning("Persistence layer appears dirty.")
			if err = p.cleanUpStage1(fingerprintToSeries); err != nil {
				sm = nil
			}
			go p.cleanUpStage2(sm)
		}
		if err == nil {
			atomic.AddInt64(&numMemChunks, chunksTotal)
			atomic.AddInt64(&numMemChunkDescs, chunkDescsTotal)
		}
	}()

	f, err := os.Open(p.headsFileName())
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		glog.Warning("Could not open heads file:", err)
		p.dirty = true
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, fileBufSize)

	buf := make([]byte, len(headsMagicString))
	if _, err := io.ReadFull(r, buf); err != nil {
		glog.Warning("Could not read from heads file:", err)
		p.dirty = true
		return sm, nil
	}
	magic := string(buf)
	if magic != headsMagicString {
		glog.Warningf(
			"unexpected magic string, want %q, got %q",
			headsMagicString, magic,
		)
		p.dirty = true
		return
	}
	if version, err := binary.ReadVarint(r); version != headsFormatVersion || err != nil {
		glog.Warningf("unknown heads format version, want %d", headsFormatVersion)
		p.dirty = true
		return sm, nil
	}
	numSeries, err := codable.DecodeUint64(r)
	if err != nil {
		glog.Warning("Could not decode number of series:", err)
		p.dirty = true
		return sm, nil
	}

	for ; numSeries > 0; numSeries-- {
		seriesFlags, err := r.ReadByte()
		if err != nil {
			glog.Warning("Could not read series flags:", err)
			p.dirty = true
			return sm, nil
		}
		headChunkPersisted := seriesFlags&flagHeadChunkPersisted != 0
		fp, err := codable.DecodeUint64(r)
		if err != nil {
			glog.Warning("Could not decode fingerprint:", err)
			p.dirty = true
			return sm, nil
		}
		var metric codable.Metric
		if err := metric.UnmarshalFromReader(r); err != nil {
			glog.Warning("Could not decode metric:", err)
			p.dirty = true
			return sm, nil
		}
		chunkDescsOffset, err := binary.ReadVarint(r)
		if err != nil {
			glog.Warning("Could not decode chunk descriptor offset:", err)
			p.dirty = true
			return sm, nil
		}
		numChunkDescs, err := binary.ReadVarint(r)
		if err != nil {
			glog.Warning("Could not decode number of chunk descriptors:", err)
			p.dirty = true
			return sm, nil
		}
		chunkDescs := make([]*chunkDesc, numChunkDescs)
		chunkDescsTotal += numChunkDescs

		for i := int64(0); i < numChunkDescs; i++ {
			if headChunkPersisted || i < numChunkDescs-1 {
				firstTime, err := binary.ReadVarint(r)
				if err != nil {
					glog.Warning("Could not decode first time:", err)
					p.dirty = true
					return sm, nil
				}
				lastTime, err := binary.ReadVarint(r)
				if err != nil {
					glog.Warning("Could not decode last time:", err)
					p.dirty = true
					return sm, nil
				}
				chunkDescs[i] = &chunkDesc{
					chunkFirstTime: clientmodel.Timestamp(firstTime),
					chunkLastTime:  clientmodel.Timestamp(lastTime),
				}
			} else {
				// Non-persisted head chunk.
				chunksTotal++
				chunkType, err := r.ReadByte()
				if err != nil {
					glog.Warning("Could not decode chunk type:", err)
					p.dirty = true
					return sm, nil
				}
				chunk := chunkForType(chunkType)
				if err := chunk.unmarshal(r); err != nil {
					glog.Warning("Could not decode chunk type:", err)
					p.dirty = true
					return sm, nil
				}
				chunkDescs[i] = newChunkDesc(chunk)
			}
		}

		fingerprintToSeries[clientmodel.Fingerprint(fp)] = &memorySeries{
			metric:             clientmodel.Metric(metric),
			chunkDescs:         chunkDescs,
			chunkDescsOffset:   int(chunkDescsOffset),
			headChunkPersisted: headChunkPersisted,
		}
	}
	return sm, nil
}

// dropChunks deletes all chunks from a series whose last sample time is before
// beforeTime. It returns the number of deleted chunks and true if all chunks of
// the series have been deleted.  It is the caller's responsibility to make sure
// nothing is persisted or loaded for the same fingerprint concurrently.
func (p *persistence) dropChunks(fp clientmodel.Fingerprint, beforeTime clientmodel.Timestamp) (int, bool, error) {
	f, err := p.openChunkFileForReading(fp)
	if os.IsNotExist(err) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()

	// Find the first chunk that should be kept.
	var i int
	for ; ; i++ {
		_, err := f.Seek(p.offsetForChunkIndex(i)+chunkHeaderLastTimeOffset, os.SEEK_SET)
		if err != nil {
			return 0, false, err
		}
		lastTimeBuf := make([]byte, 8)
		_, err = io.ReadAtLeast(f, lastTimeBuf, 8)
		if err == io.EOF {
			// We ran into the end of the file without finding any chunks that should
			// be kept. Remove the whole file.
			chunkOps.WithLabelValues(purge).Add(float64(i))
			if err := os.Remove(f.Name()); err != nil {
				return 0, true, err
			}
			return i, true, nil
		}
		if err != nil {
			return 0, false, err
		}
		lastTime := clientmodel.Timestamp(binary.LittleEndian.Uint64(lastTimeBuf))
		if !lastTime.Before(beforeTime) {
			chunkOps.WithLabelValues(purge).Add(float64(i))
			break
		}
	}

	// We've found the first chunk that should be kept. Seek backwards to the
	// beginning of its header and start copying everything from there into a new
	// file.
	_, err = f.Seek(-(chunkHeaderLastTimeOffset + 8), os.SEEK_CUR)
	if err != nil {
		return 0, false, err
	}

	temp, err := os.OpenFile(p.tempFileNameForFingerprint(fp), os.O_WRONLY|os.O_CREATE, 0640)
	if err != nil {
		return 0, false, err
	}
	defer temp.Close()

	if _, err := io.Copy(temp, f); err != nil {
		return 0, false, err
	}

	if err := os.Rename(p.tempFileNameForFingerprint(fp), p.fileNameForFingerprint(fp)); err != nil {
		return 0, false, err
	}
	return i, false, nil
}

// indexMetric queues the given metric for addition to the indexes needed by
// getFingerprintsForLabelPair, getLabelValuesForLabelName, and
// getFingerprintsModifiedBefore.  If the queue is full, this method blocks
// until the metric can be queued.  This method is goroutine-safe.
func (p *persistence) indexMetric(fp clientmodel.Fingerprint, m clientmodel.Metric) {
	p.indexingQueue <- indexingOp{fp, m, add}
}

// unindexMetric queues references to the given metric for removal from the
// indexes used for getFingerprintsForLabelPair, getLabelValuesForLabelName, and
// getFingerprintsModifiedBefore. The index of fingerprints to archived metrics
// is not affected by this removal. (In fact, never call this method for an
// archived metric. To drop an archived metric, call dropArchivedFingerprint.)
// If the queue is full, this method blocks until the metric can be queued. This
// method is goroutine-safe.
func (p *persistence) unindexMetric(fp clientmodel.Fingerprint, m clientmodel.Metric) {
	p.indexingQueue <- indexingOp{fp, m, remove}
}

// waitForIndexing waits until all items in the indexing queue are processed. If
// queue processing is currently on hold (to gather more ops for batching), this
// method will trigger an immediate start of processing. This method is
// goroutine-safe.
func (p *persistence) waitForIndexing() {
	wait := make(chan int)
	for {
		p.indexingFlush <- wait
		if <-wait == 0 {
			break
		}
	}
}

// archiveMetric persists the mapping of the given fingerprint to the given
// metric, together with the first and last timestamp of the series belonging to
// the metric. This method is goroutine-safe.
func (p *persistence) archiveMetric(
	fp clientmodel.Fingerprint, m clientmodel.Metric, first, last clientmodel.Timestamp,
) error {
	p.archiveMtx.Lock()
	defer p.archiveMtx.Unlock()

	if err := p.archivedFingerprintToMetrics.Put(codable.Fingerprint(fp), codable.Metric(m)); err != nil {
		return err
	}
	if err := p.archivedFingerprintToTimeRange.Put(codable.Fingerprint(fp), codable.TimeRange{First: first, Last: last}); err != nil {
		return err
	}
	return nil
}

// hasArchivedMetric returns whether the archived metric for the given
// fingerprint exists and if yes, what the first and last timestamp in the
// corresponding series is. This method is goroutine-safe.
func (p *persistence) hasArchivedMetric(fp clientmodel.Fingerprint) (
	hasMetric bool, firstTime, lastTime clientmodel.Timestamp, err error,
) {
	firstTime, lastTime, hasMetric, err = p.archivedFingerprintToTimeRange.Lookup(fp)
	return
}

// getFingerprintsModifiedBefore returns the fingerprints of archived timeseries
// that have live samples before the provided timestamp. This method is
// goroutine-safe.
func (p *persistence) getFingerprintsModifiedBefore(beforeTime clientmodel.Timestamp) ([]clientmodel.Fingerprint, error) {
	// The locking makes sure archivedFingerprintToTimeRange won't be
	// mutated while being iterated over (which will probably not result in
	// races, but might still yield weird results).
	p.archiveMtx.Lock()
	defer p.archiveMtx.Unlock()

	var fp codable.Fingerprint
	var tr codable.TimeRange
	fps := []clientmodel.Fingerprint{}
	p.archivedFingerprintToTimeRange.ForEach(func(kv index.KeyValueAccessor) error {
		if err := kv.Value(&tr); err != nil {
			return err
		}
		if tr.First.Before(beforeTime) {
			if err := kv.Key(&fp); err != nil {
				return err
			}
			fps = append(fps, clientmodel.Fingerprint(fp))
		}
		return nil
	})
	return fps, nil
}

// getArchivedMetric retrieves the archived metric with the given
// fingerprint. This method is goroutine-safe.
func (p *persistence) getArchivedMetric(fp clientmodel.Fingerprint) (clientmodel.Metric, error) {
	metric, _, err := p.archivedFingerprintToMetrics.Lookup(fp)
	return metric, err
}

// dropArchivedMetric deletes an archived fingerprint and its corresponding
// metric entirely. It also queues the metric for un-indexing (no need to call
// unindexMetric for the deleted metric.)  This method is goroutine-safe.
func (p *persistence) dropArchivedMetric(fp clientmodel.Fingerprint) error {
	p.archiveMtx.Lock()
	defer p.archiveMtx.Unlock()

	metric, err := p.getArchivedMetric(fp)
	if err != nil || metric == nil {
		return err
	}
	if err := p.archivedFingerprintToMetrics.Delete(codable.Fingerprint(fp)); err != nil {
		return err
	}
	if err := p.archivedFingerprintToTimeRange.Delete(codable.Fingerprint(fp)); err != nil {
		return err
	}
	p.unindexMetric(fp, metric)
	return nil
}

// unarchiveMetric deletes an archived fingerprint and its metric, but (in
// contrast to dropArchivedMetric) does not un-index the metric.  The method
// returns true if a metric was actually deleted. This method is goroutine-safe.
func (p *persistence) unarchiveMetric(fp clientmodel.Fingerprint) (bool, error) {
	p.archiveMtx.Lock()
	defer p.archiveMtx.Unlock()

	has, err := p.archivedFingerprintToTimeRange.Has(fp)
	if err != nil || !has {
		return false, err
	}
	if err := p.archivedFingerprintToMetrics.Delete(codable.Fingerprint(fp)); err != nil {
		return false, err
	}
	if err := p.archivedFingerprintToTimeRange.Delete(codable.Fingerprint(fp)); err != nil {
		return false, err
	}
	return true, nil
}

// close flushes the indexing queue and other buffered data and releases any
// held resources. It also removes the dirty marker file if successful and if
// the persistence is currently not marked as dirty.
func (p *persistence) close() error {
	close(p.indexingQueue)
	<-p.indexingStopped

	var lastError error
	if err := p.archivedFingerprintToMetrics.Close(); err != nil {
		lastError = err
		glog.Error("Error closing archivedFingerprintToMetric index DB: ", err)
	}
	if err := p.archivedFingerprintToTimeRange.Close(); err != nil {
		lastError = err
		glog.Error("Error closing archivedFingerprintToTimeRange index DB: ", err)
	}
	if err := p.labelPairToFingerprints.Close(); err != nil {
		lastError = err
		glog.Error("Error closing labelPairToFingerprints index DB: ", err)
	}
	if err := p.labelNameToLabelValues.Close(); err != nil {
		lastError = err
		glog.Error("Error closing labelNameToLabelValues index DB: ", err)
	}
	if lastError == nil && !p.dirty {
		lastError = os.Remove(p.dirtyFileName())
	}
	return lastError
}

func (p *persistence) dirNameForFingerprint(fp clientmodel.Fingerprint) string {
	fpStr := fp.String()
	return path.Join(p.basePath, fpStr[0:2])
}

func (p *persistence) fileNameForFingerprint(fp clientmodel.Fingerprint) string {
	fpStr := fp.String()
	return path.Join(p.basePath, fpStr[0:2], fpStr[2:]+seriesFileSuffix)
}

func (p *persistence) tempFileNameForFingerprint(fp clientmodel.Fingerprint) string {
	fpStr := fp.String()
	return path.Join(p.basePath, fpStr[0:2], fpStr[2:]+seriesTempFileSuffix)
}

func (p *persistence) openChunkFileForWriting(fp clientmodel.Fingerprint) (*os.File, error) {
	if err := os.MkdirAll(p.dirNameForFingerprint(fp), 0700); err != nil {
		return nil, err
	}
	return os.OpenFile(p.fileNameForFingerprint(fp), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0640)
}

func (p *persistence) openChunkFileForReading(fp clientmodel.Fingerprint) (*os.File, error) {
	return os.Open(p.fileNameForFingerprint(fp))
}

func writeChunkHeader(w io.Writer, c chunk) error {
	header := make([]byte, chunkHeaderLen)
	header[chunkHeaderTypeOffset] = chunkType(c)
	binary.LittleEndian.PutUint64(header[chunkHeaderFirstTimeOffset:], uint64(c.firstTime()))
	binary.LittleEndian.PutUint64(header[chunkHeaderLastTimeOffset:], uint64(c.lastTime()))
	_, err := w.Write(header)
	return err
}

func (p *persistence) offsetForChunkIndex(i int) int64 {
	return int64(i * (chunkHeaderLen + p.chunkLen))
}

func (p *persistence) chunkIndexForOffset(offset int64) (int, error) {
	if int(offset)%(chunkHeaderLen+p.chunkLen) != 0 {
		return -1, fmt.Errorf(
			"offset %d is not a multiple of on-disk chunk length %d",
			offset, chunkHeaderLen+p.chunkLen,
		)
	}
	return int(offset) / (chunkHeaderLen + p.chunkLen), nil
}

func (p *persistence) headsFileName() string {
	return path.Join(p.basePath, headsFileName)
}

func (p *persistence) headsTempFileName() string {
	return path.Join(p.basePath, headsTempFileName)
}

func (p *persistence) processIndexingQueue() {
	batchSize := 0
	nameToValues := index.LabelNameLabelValuesMapping{}
	pairToFPs := index.LabelPairFingerprintsMapping{}
	batchTimeout := time.NewTimer(indexingBatchTimeout)
	defer batchTimeout.Stop()

	commitBatch := func() {
		p.indexingBatchSizes.Observe(float64(batchSize))
		defer func(begin time.Time) {
			p.indexingBatchLatency.Observe(float64(time.Since(begin) / time.Millisecond))
		}(time.Now())

		if err := p.labelPairToFingerprints.IndexBatch(pairToFPs); err != nil {
			glog.Error("Error indexing label pair to fingerprints batch: ", err)
		}
		if err := p.labelNameToLabelValues.IndexBatch(nameToValues); err != nil {
			glog.Error("Error indexing label name to label values batch: ", err)
		}
		batchSize = 0
		nameToValues = index.LabelNameLabelValuesMapping{}
		pairToFPs = index.LabelPairFingerprintsMapping{}
		batchTimeout.Reset(indexingBatchTimeout)
	}

	var flush chan chan int
loop:
	for {
		// Only process flush requests if the queue is currently empty.
		if len(p.indexingQueue) == 0 {
			flush = p.indexingFlush
		} else {
			flush = nil
		}
		select {
		case <-batchTimeout.C:
			// Only commit if we have something to commit _and_
			// nothing is waiting in the queue to be picked up. That
			// prevents a death spiral if the LookupSet calls below
			// are slow for some reason.
			if batchSize > 0 && len(p.indexingQueue) == 0 {
				commitBatch()
			} else {
				batchTimeout.Reset(indexingBatchTimeout)
			}
		case r := <-flush:
			if batchSize > 0 {
				commitBatch()
			}
			r <- len(p.indexingQueue)
		case op, ok := <-p.indexingQueue:
			if !ok {
				if batchSize > 0 {
					commitBatch()
				}
				break loop
			}

			batchSize++
			for ln, lv := range op.metric {
				lp := metric.LabelPair{Name: ln, Value: lv}
				baseFPs, ok := pairToFPs[lp]
				if !ok {
					var err error
					baseFPs, _, err = p.labelPairToFingerprints.LookupSet(lp)
					if err != nil {
						glog.Errorf("Error looking up label pair %v: %s", lp, err)
						continue
					}
					pairToFPs[lp] = baseFPs
				}
				baseValues, ok := nameToValues[ln]
				if !ok {
					var err error
					baseValues, _, err = p.labelNameToLabelValues.LookupSet(ln)
					if err != nil {
						glog.Errorf("Error looking up label name %v: %s", ln, err)
						continue
					}
					nameToValues[ln] = baseValues
				}
				switch op.opType {
				case add:
					baseFPs[op.fingerprint] = struct{}{}
					baseValues[lv] = struct{}{}
				case remove:
					delete(baseFPs, op.fingerprint)
					if len(baseFPs) == 0 {
						delete(baseValues, lv)
					}
				default:
					panic("unknown op type")
				}
			}

			if batchSize >= indexingMaxBatchSize {
				commitBatch()
			}
		}
	}
	close(p.indexingStopped)
}

// exists returns true when the given file or directory exists.
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}

	return false, err
}
