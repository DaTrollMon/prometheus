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
	"time"

	"github.com/golang/glog"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/storage/local/codable"
	"github.com/prometheus/prometheus/storage/local/index"
	"github.com/prometheus/prometheus/storage/metric"
)

const (
	seriesFileName     = "series.db"
	seriesTempFileName = "series.db.tmp"

	headsFileName      = "heads.db"
	headsFormatVersion = 1
	headsMagicString   = "PrometheusHeads"

	fileBufSize = 1 << 16 // 64kiB. TODO: Tweak.

	chunkHeaderLen             = 17
	chunkHeaderTypeOffset      = 0
	chunkHeaderFirstTimeOffset = 1
	chunkHeaderLastTimeOffset  = 9

	indexingMaxBatchSize  = 1024
	indexingBatchTimeout  = 500 * time.Millisecond    // Commit batch when idle for that long.
	indexingQueueCapacity = 10 * indexingMaxBatchSize // TODO: Export as metric.
)

const (
	_                         = iota
	flagChunkDescsLoaded byte = 1 << iota
	flagHeadChunkPersisted
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

type diskPersistence struct {
	basePath string
	chunkLen int

	archivedFingerprintToMetrics   *index.FingerprintMetricIndex
	archivedFingerprintToTimeRange *index.FingerprintTimeRangeIndex
	labelPairToFingerprints        *index.LabelPairFingerprintIndex
	labelNameToLabelValues         *index.LabelNameLabelValuesIndex

	indexingQueue   chan indexingOp
	indexingStopped chan struct{}
	indexingFlush   chan chan int
}

// NewDiskPersistence returns a newly allocated Persistence backed by local disk storage, ready to use.
func NewDiskPersistence(basePath string, chunkLen int) (Persistence, error) {
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

	p := &diskPersistence{
		basePath: basePath,
		chunkLen: chunkLen,

		archivedFingerprintToMetrics:   archivedFingerprintToMetrics,
		archivedFingerprintToTimeRange: archivedFingerprintToTimeRange,
		labelPairToFingerprints:        labelPairToFingerprints,
		labelNameToLabelValues:         labelNameToLabelValues,

		indexingQueue:   make(chan indexingOp, indexingQueueCapacity),
		indexingStopped: make(chan struct{}),
		indexingFlush:   make(chan chan int),
	}
	go p.processIndexingQueue()
	return p, nil
}

// GetFingerprintsForLabelPair implements persistence.
func (p *diskPersistence) GetFingerprintsForLabelPair(lp metric.LabelPair) (clientmodel.Fingerprints, error) {
	fps, _, err := p.labelPairToFingerprints.Lookup(lp)
	if err != nil {
		return nil, err
	}
	return fps, nil
}

// GetLabelValuesForLabelName implements persistence.
func (p *diskPersistence) GetLabelValuesForLabelName(ln clientmodel.LabelName) (clientmodel.LabelValues, error) {
	lvs, _, err := p.labelNameToLabelValues.Lookup(ln)
	if err != nil {
		return nil, err
	}
	return lvs, nil
}

// PersistChunk implements Persistence.
func (p *diskPersistence) PersistChunk(fp clientmodel.Fingerprint, c chunk) error {
	// 1. Open chunk file.
	f, err := p.openChunkFileForWriting(fp)
	if err != nil {
		return err
	}
	defer f.Close()

	b := bufio.NewWriterSize(f, chunkHeaderLen+p.chunkLen)
	defer b.Flush()

	// 2. Write the header (chunk type and first/last times).
	err = writeChunkHeader(b, c)
	if err != nil {
		return err
	}

	// 3. Write chunk into file.
	return c.marshal(b)
}

// LoadChunks implements Persistence.
func (p *diskPersistence) LoadChunks(fp clientmodel.Fingerprint, indexes []int) (chunks, error) {
	// TODO: we need to verify at some point that file length is a multiple of
	// the chunk size. When is the best time to do this, and where to remember
	// it? Right now, we only do it when loading chunkDescs.
	f, err := p.openChunkFileForReading(fp)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	chunks := make(chunks, 0, len(indexes))
	defer func() {
		if err == nil {
			return
		}
	}()

	typeBuf := make([]byte, 1)
	for _, idx := range indexes {
		_, err := f.Seek(p.offsetForChunkIndex(idx), os.SEEK_SET)
		if err != nil {
			return nil, err
		}
		// TODO: check seek offset too?

		n, err := f.Read(typeBuf)
		if err != nil {
			return nil, err
		}
		if n != 1 {
			// Shouldn't happen?
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

func (p *diskPersistence) LoadChunkDescs(fp clientmodel.Fingerprint, beforeTime clientmodel.Timestamp) (chunkDescs, error) {
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
	cds := make(chunkDescs, 0, numChunks)
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
			firstTimeField: clientmodel.Timestamp(binary.LittleEndian.Uint64(chunkTimesBuf)),
			lastTimeField:  clientmodel.Timestamp(binary.LittleEndian.Uint64(chunkTimesBuf[8:])),
		}
		if !cd.firstTime().Before(beforeTime) {
			// From here on, we have chunkDescs in memory already.
			break
		}
		cds = append(cds, cd)
	}
	return cds, nil
}

// PersistSeriesMapAndHeads implements Persistence.
func (p *diskPersistence) PersistSeriesMapAndHeads(fingerprintToSeries SeriesMap) error {
	f, err := os.OpenFile(p.headsPath(), os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0640)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, fileBufSize)

	if _, err := w.WriteString(headsMagicString); err != nil {
		return err
	}
	if err := codable.EncodeVarint(w, headsFormatVersion); err != nil {
		return err
	}
	if err := codable.EncodeVarint(w, int64(len(fingerprintToSeries))); err != nil {
		return err
	}

	for fp, series := range fingerprintToSeries {
		var seriesFlags byte
		if series.chunkDescsLoaded {
			seriesFlags |= flagChunkDescsLoaded
		}
		if series.headChunkPersisted {
			seriesFlags |= flagHeadChunkPersisted
		}
		if err := w.WriteByte(seriesFlags); err != nil {
			return err
		}
		if err := codable.EncodeUint64(w, uint64(fp)); err != nil {
			return err
		}
		buf, err := codable.Metric(series.metric).MarshalBinary()
		if err != nil {
			return err
		}
		w.Write(buf)
		if err := codable.EncodeVarint(w, int64(len(series.chunkDescs))); err != nil {
			return err
		}
		for i, chunkDesc := range series.chunkDescs {
			if series.headChunkPersisted || i < len(series.chunkDescs)-1 {
				if err := codable.EncodeVarint(w, int64(chunkDesc.firstTime())); err != nil {
					return err
				}
				if err := codable.EncodeVarint(w, int64(chunkDesc.lastTime())); err != nil {
					return err
				}
			} else {
				// This is the non-persisted head chunk. Fully marshal it.
				if err := w.WriteByte(chunkType(chunkDesc.chunk)); err != nil {
					return err
				}
				if err := chunkDesc.chunk.marshal(w); err != nil {
					return err
				}
			}
		}
	}
	return w.Flush()
}

// LoadSeriesMapAndHeads implements Persistence.
func (p *diskPersistence) LoadSeriesMapAndHeads() (SeriesMap, error) {
	f, err := os.Open(p.headsPath())
	if os.IsNotExist(err) {
		return SeriesMap{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, fileBufSize)

	buf := make([]byte, len(headsMagicString))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	magic := string(buf)
	if magic != headsMagicString {
		return nil, fmt.Errorf(
			"unexpected magic string, want %q, got %q",
			headsMagicString, magic,
		)
	}
	if version, err := binary.ReadVarint(r); version != headsFormatVersion || err != nil {
		return nil, fmt.Errorf("unknown heads format version, want %d", headsFormatVersion)
	}
	numSeries, err := binary.ReadVarint(r)
	if err != nil {
		return nil, err
	}
	fingerprintToSeries := make(SeriesMap, numSeries)

	for ; numSeries > 0; numSeries-- {
		seriesFlags, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		headChunkPersisted := seriesFlags&flagHeadChunkPersisted != 0
		fp, err := codable.DecodeUint64(r)
		if err != nil {
			return nil, err
		}
		var metric codable.Metric
		if err := metric.UnmarshalFromReader(r); err != nil {
			return nil, err
		}
		numChunkDescs, err := binary.ReadVarint(r)
		if err != nil {
			return nil, err
		}
		chunkDescs := make(chunkDescs, numChunkDescs)

		for i := int64(0); i < numChunkDescs; i++ {
			if headChunkPersisted || i < numChunkDescs-1 {
				firstTime, err := binary.ReadVarint(r)
				if err != nil {
					return nil, err
				}
				lastTime, err := binary.ReadVarint(r)
				if err != nil {
					return nil, err
				}
				chunkDescs[i] = &chunkDesc{
					firstTimeField: clientmodel.Timestamp(firstTime),
					lastTimeField:  clientmodel.Timestamp(lastTime),
				}
			} else {
				// Non-persisted head chunk.
				chunkType, err := r.ReadByte()
				if err != nil {
					return nil, err
				}
				chunk := chunkForType(chunkType)
				if err := chunk.unmarshal(r); err != nil {
					return nil, err
				}
				chunkDescs[i] = &chunkDesc{
					chunk:    chunk,
					refCount: 1,
				}
			}
		}

		fingerprintToSeries[clientmodel.Fingerprint(fp)] = &memorySeries{
			metric:             clientmodel.Metric(metric),
			chunkDescs:         chunkDescs,
			chunkDescsLoaded:   seriesFlags&flagChunkDescsLoaded != 0,
			headChunkPersisted: headChunkPersisted,
		}
	}
	return fingerprintToSeries, nil
}

// DropChunks implements persistence.
func (p *diskPersistence) DropChunks(fp clientmodel.Fingerprint, beforeTime clientmodel.Timestamp) (bool, error) {
	f, err := p.openChunkFileForReading(fp)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()

	// Find the first chunk that should be kept.
	for i := 0; ; i++ {
		_, err := f.Seek(p.offsetForChunkIndex(i)+chunkHeaderLastTimeOffset, os.SEEK_SET)
		if err != nil {
			return false, err
		}
		lastTimeBuf := make([]byte, 8)
		_, err = io.ReadAtLeast(f, lastTimeBuf, 8)
		if err == io.EOF {
			// We ran into the end of the file without finding any chunks that should
			// be kept. Remove the whole file.
			if err := os.Remove(f.Name()); err != nil {
				return true, err
			}
			return true, nil
		}
		if err != nil {
			return false, err
		}
		lastTime := clientmodel.Timestamp(binary.LittleEndian.Uint64(lastTimeBuf))
		if !lastTime.Before(beforeTime) {
			break
		}
	}

	// We've found the first chunk that should be kept. Seek backwards to the
	// beginning of its header and start copying everything from there into a new
	// file.
	_, err = f.Seek(-(chunkHeaderLastTimeOffset + 8), os.SEEK_CUR)
	if err != nil {
		return false, err
	}

	dirname := p.dirForFingerprint(fp)
	temp, err := os.OpenFile(path.Join(dirname, seriesTempFileName), os.O_WRONLY|os.O_CREATE, 0640)
	if err != nil {
		return false, err
	}
	defer temp.Close()

	if _, err := io.Copy(temp, f); err != nil {
		return false, err
	}

	os.Rename(path.Join(dirname, seriesTempFileName), path.Join(dirname, seriesFileName))
	return false, nil
}

// IndexMetric implements Persistence.
func (p *diskPersistence) IndexMetric(m clientmodel.Metric, fp clientmodel.Fingerprint) {
	p.indexingQueue <- indexingOp{fp, m, add}
}

// UnindexMetric implements Persistence.
func (p *diskPersistence) UnindexMetric(m clientmodel.Metric, fp clientmodel.Fingerprint) {
	p.indexingQueue <- indexingOp{fp, m, remove}
}

// WaitForIndexing implements Persistence.
func (p *diskPersistence) WaitForIndexing() {
	wait := make(chan int)
	for {
		p.indexingFlush <- wait
		if <-wait == 0 {
			break
		}
	}
}

// ArchiveMetric implements Persistence.
func (p *diskPersistence) ArchiveMetric(
	fp clientmodel.Fingerprint, m clientmodel.Metric, first, last clientmodel.Timestamp,
) error {
	if err := p.archivedFingerprintToMetrics.Put(codable.Fingerprint(fp), codable.Metric(m)); err != nil {
		return err
	}
	if err := p.archivedFingerprintToTimeRange.Put(codable.Fingerprint(fp), codable.TimeRange{First: first, Last: last}); err != nil {
		return err
	}
	return nil
}

// HasArchivedMetric implements Persistence.
func (p *diskPersistence) HasArchivedMetric(fp clientmodel.Fingerprint) (
	hasMetric bool, firstTime, lastTime clientmodel.Timestamp, err error,
) {
	firstTime, lastTime, hasMetric, err = p.archivedFingerprintToTimeRange.Lookup(fp)
	return
}

// GetFingerprintsModifiedBefore implements Persistence.
func (p *diskPersistence) GetFingerprintsModifiedBefore(beforeTime clientmodel.Timestamp) ([]clientmodel.Fingerprint, error) {
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

// GetArchivedMetric implements Persistence.
func (p *diskPersistence) GetArchivedMetric(fp clientmodel.Fingerprint) (clientmodel.Metric, error) {
	metric, _, err := p.archivedFingerprintToMetrics.Lookup(fp)
	return metric, err
}

// DropArchivedMetric implements Persistence.
func (p *diskPersistence) DropArchivedMetric(fp clientmodel.Fingerprint) error {
	metric, err := p.GetArchivedMetric(fp)
	if err != nil || metric == nil {
		return err
	}
	if err := p.archivedFingerprintToMetrics.Delete(codable.Fingerprint(fp)); err != nil {
		return err
	}
	if err := p.archivedFingerprintToTimeRange.Delete(codable.Fingerprint(fp)); err != nil {
		return err
	}
	p.UnindexMetric(metric, fp)
	return nil
}

// UnarchiveMetric implements Persistence.
func (p *diskPersistence) UnarchiveMetric(fp clientmodel.Fingerprint) (bool, error) {
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

// Close implements Persistence.
func (p *diskPersistence) Close() error {
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
	return lastError
}

func (p *diskPersistence) dirForFingerprint(fp clientmodel.Fingerprint) string {
	fpStr := fp.String()
	return fmt.Sprintf("%s/%c%c/%s", p.basePath, fpStr[0], fpStr[1], fpStr[2:])
}

func (p *diskPersistence) openChunkFileForWriting(fp clientmodel.Fingerprint) (*os.File, error) {
	dirname := p.dirForFingerprint(fp)
	ex, err := exists(dirname)
	if err != nil {
		return nil, err
	}
	if !ex {
		if err := os.MkdirAll(dirname, 0700); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path.Join(dirname, seriesFileName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0640)
}

func (p *diskPersistence) openChunkFileForReading(fp clientmodel.Fingerprint) (*os.File, error) {
	dirname := p.dirForFingerprint(fp)
	return os.Open(path.Join(dirname, seriesFileName))
}

func writeChunkHeader(w io.Writer, c chunk) error {
	header := make([]byte, chunkHeaderLen)
	header[chunkHeaderTypeOffset] = chunkType(c)
	binary.LittleEndian.PutUint64(header[chunkHeaderFirstTimeOffset:], uint64(c.firstTime()))
	binary.LittleEndian.PutUint64(header[chunkHeaderLastTimeOffset:], uint64(c.lastTime()))
	_, err := w.Write(header)
	return err
}

func (p *diskPersistence) offsetForChunkIndex(i int) int64 {
	return int64(i * (chunkHeaderLen + p.chunkLen))
}

func (p *diskPersistence) headsPath() string {
	return path.Join(p.basePath, headsFileName)
}

func (p *diskPersistence) processIndexingQueue() {
	batchSize := 0
	nameToValues := index.LabelNameLabelValuesMapping{}
	pairToFPs := index.LabelPairFingerprintsMapping{}
	batchTimeout := time.NewTimer(indexingBatchTimeout)
	defer batchTimeout.Stop()

	commitBatch := func() {
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
			if batchSize > 0 {
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
