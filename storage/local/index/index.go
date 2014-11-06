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

// Package index provides a number of indexes backed by persistent key-value
// stores.  The only supported implementation of a key-value store is currently
// goleveldb, but other implementations can easily be added.
package index

import (
	"flag"
	"path"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/storage/local/codable"
	"github.com/prometheus/prometheus/storage/metric"
)

const (
	fingerprintToMetricDir     = "archived_fingerprint_to_metric"
	fingerprintTimeRangeDir    = "archived_fingerprint_to_timerange"
	labelNameToLabelValuesDir  = "labelname_to_labelvalues"
	labelPairToFingerprintsDir = "labelpair_to_fingerprints"
)

var (
	fingerprintToMetricCacheSize     = flag.Int("storage.fingerprintToMetricCacheSizeBytes", 25*1024*1024, "The size in bytes for the fingerprint to metric index cache.")
	labelNameToLabelValuesCacheSize  = flag.Int("storage.labelNameToLabelValuesCacheSizeBytes", 25*1024*1024, "The size in bytes for the label name to label values index cache.")
	labelPairToFingerprintsCacheSize = flag.Int("storage.labelPairToFingerprintsCacheSizeBytes", 25*1024*1024, "The size in bytes for the label pair to fingerprints index cache.")
	fingerprintTimeRangeCacheSize    = flag.Int("storage.fingerprintTimeRangeCacheSizeBytes", 5*1024*1024, "The size in bytes for the metric time range index cache.")
)

// FingerprintMetricMapping is an in-memory map of fingerprints to metrics.
type FingerprintMetricMapping map[clientmodel.Fingerprint]clientmodel.Metric

// FingerprintMetricIndex models a database mapping fingerprints to metrics.
type FingerprintMetricIndex struct {
	KeyValueStore
}

// IndexBatch indexes a batch of mappings from fingerprints to metrics.
//
// This method is goroutine-safe, but note that no specific order of execution
// can be guaranteed (especially critical if IndexBatch and UnindexBatch are
// called concurrently).
func (i *FingerprintMetricIndex) IndexBatch(mapping FingerprintMetricMapping) error {
	b := i.NewBatch()

	for fp, m := range mapping {
		b.Put(codable.Fingerprint(fp), codable.Metric(m))
	}

	return i.Commit(b)
}

// UnindexBatch unindexes a batch of mappings from fingerprints to metrics.
//
// This method is goroutine-safe, but note that no specific order of execution
// can be guaranteed (especially critical if IndexBatch and UnindexBatch are
// called concurrently).
func (i *FingerprintMetricIndex) UnindexBatch(mapping FingerprintMetricMapping) error {
	b := i.NewBatch()

	for fp := range mapping {
		b.Delete(codable.Fingerprint(fp))
	}

	return i.Commit(b)
}

// Lookup looks up a metric by fingerprint. Looking up a non-existing
// fingerprint is not an error. In that case, (nil, false, nil) is returned.
//
// This method is goroutine-safe.
func (i *FingerprintMetricIndex) Lookup(fp clientmodel.Fingerprint) (metric clientmodel.Metric, ok bool, err error) {
	ok, err = i.Get(codable.Fingerprint(fp), (*codable.Metric)(&metric))
	return
}

// NewFingerprintMetricIndex returns a LevelDB-backed FingerprintMetricIndex
// ready to use.
func NewFingerprintMetricIndex(basePath string) (*FingerprintMetricIndex, error) {
	fingerprintToMetricDB, err := NewLevelDB(LevelDBOptions{
		Path:           path.Join(basePath, fingerprintToMetricDir),
		CacheSizeBytes: *fingerprintToMetricCacheSize,
	})
	if err != nil {
		return nil, err
	}
	return &FingerprintMetricIndex{
		KeyValueStore: fingerprintToMetricDB,
	}, nil
}

// LabelNameLabelValuesMapping is an in-memory map of label names to
// label values.
type LabelNameLabelValuesMapping map[clientmodel.LabelName]codable.LabelValueSet

// LabelNameLabelValuesIndex is a KeyValueStore that maps existing label names
// to all label values stored for that label name.
type LabelNameLabelValuesIndex struct {
	KeyValueStore
}

// IndexBatch adds a batch of label name to label values mappings to the
// index. A mapping of a label name to an empty slice of label values results in
// a deletion of that mapping from the index.
//
// While this method is fundamentally goroutine-safe, note that the order of
// execution for multiple batches executed concurrently is undefined.
func (i *LabelNameLabelValuesIndex) IndexBatch(b LabelNameLabelValuesMapping) error {
	batch := i.NewBatch()

	for name, values := range b {
		if len(values) == 0 {
			if err := batch.Delete(codable.LabelName(name)); err != nil {
				return err
			}
		} else {
			if err := batch.Put(codable.LabelName(name), values); err != nil {
				return err
			}
		}
	}

	return i.Commit(batch)
}

// Lookup looks up all label values for a given label name. Looking up a
// non-existing label name is not an error. In that case, (nil, false, nil) is
// returned.
//
// This method is goroutine-safe.
func (i *LabelNameLabelValuesIndex) Lookup(l clientmodel.LabelName) (values clientmodel.LabelValues, ok bool, err error) {
	ok, err = i.Get(codable.LabelName(l), (*codable.LabelValues)(&values))
	return
}

// LookupSet looks up all label values for a given label name. Looking up a
// non-existing label name is not an error. In that case, (nil, false, nil) is
// returned.
//
// This method is goroutine-safe.
func (i *LabelNameLabelValuesIndex) LookupSet(l clientmodel.LabelName) (values map[clientmodel.LabelValue]struct{}, ok bool, err error) {
	ok, err = i.Get(codable.LabelName(l), (*codable.LabelValueSet)(&values))
	if values == nil {
		values = map[clientmodel.LabelValue]struct{}{}
	}
	return
}

// NewLabelNameLabelValuesIndex returns a LevelDB-backed
// LabelNameLabelValuesIndex ready to use.
func NewLabelNameLabelValuesIndex(basePath string) (*LabelNameLabelValuesIndex, error) {
	labelNameToLabelValuesDB, err := NewLevelDB(LevelDBOptions{
		Path:           path.Join(basePath, labelNameToLabelValuesDir),
		CacheSizeBytes: *labelNameToLabelValuesCacheSize,
	})
	if err != nil {
		return nil, err
	}
	return &LabelNameLabelValuesIndex{
		KeyValueStore: labelNameToLabelValuesDB,
	}, nil
}

// LabelPairFingerprintsMapping is an in-memory map of label pairs to
// fingerprints.
type LabelPairFingerprintsMapping map[metric.LabelPair]codable.FingerprintSet

// LabelPairFingerprintIndex is a KeyValueStore that maps existing label pairs
// to the fingerprints of all metrics containing those label pairs.
type LabelPairFingerprintIndex struct {
	KeyValueStore
}

// IndexBatch indexes a batch of mappings from label pairs to fingerprints. A
// mapping to an empty slice of fingerprints results in deletion of that mapping
// from the index.
//
// While this method is fundamentally goroutine-safe, note that the order of
// execution for multiple batches executed concurrently is undefined.
func (i *LabelPairFingerprintIndex) IndexBatch(m LabelPairFingerprintsMapping) error {
	batch := i.NewBatch()

	for pair, fps := range m {
		if len(fps) == 0 {
			batch.Delete(codable.LabelPair(pair))
		} else {
			batch.Put(codable.LabelPair(pair), fps)
		}
	}

	return i.Commit(batch)
}

// Lookup looks up all fingerprints for a given label pair.  Looking up a
// non-existing label pair is not an error. In that case, (nil, false, nil) is
// returned.
//
// This method is goroutine-safe.
func (i *LabelPairFingerprintIndex) Lookup(p metric.LabelPair) (fps clientmodel.Fingerprints, ok bool, err error) {
	ok, err = i.Get((codable.LabelPair)(p), (*codable.Fingerprints)(&fps))
	return
}

// LookupSet looks up all fingerprints for a given label pair.  Looking up a
// non-existing label pair is not an error. In that case, (nil, false, nil) is
// returned.
//
// This method is goroutine-safe.
func (i *LabelPairFingerprintIndex) LookupSet(p metric.LabelPair) (fps map[clientmodel.Fingerprint]struct{}, ok bool, err error) {
	ok, err = i.Get((codable.LabelPair)(p), (*codable.FingerprintSet)(&fps))
	if fps == nil {
		fps = map[clientmodel.Fingerprint]struct{}{}
	}
	return
}

// NewLabelPairFingerprintIndex returns a LevelDB-backed
// LabelPairFingerprintIndex ready to use.
func NewLabelPairFingerprintIndex(basePath string) (*LabelPairFingerprintIndex, error) {
	labelPairToFingerprintsDB, err := NewLevelDB(LevelDBOptions{
		Path:           path.Join(basePath, labelPairToFingerprintsDir),
		CacheSizeBytes: *labelPairToFingerprintsCacheSize,
	})
	if err != nil {
		return nil, err
	}
	return &LabelPairFingerprintIndex{
		KeyValueStore: labelPairToFingerprintsDB,
	}, nil
}

// FingerprintTimeRangeIndex models a database tracking the time ranges
// of metrics by their fingerprints.
type FingerprintTimeRangeIndex struct {
	KeyValueStore
}

// Lookup returns the time range for the given fingerprint.  Looking up a
// non-existing fingerprint is not an error. In that case, (0, 0, false, nil) is
// returned.
//
// This method is goroutine-safe.
func (i *FingerprintTimeRangeIndex) Lookup(fp clientmodel.Fingerprint) (firstTime, lastTime clientmodel.Timestamp, ok bool, err error) {
	var tr codable.TimeRange
	ok, err = i.Get(codable.Fingerprint(fp), &tr)
	return tr.First, tr.Last, ok, err
}

// NewFingerprintTimeRangeIndex returns a LevelDB-backed
// FingerprintTimeRangeIndex ready to use.
func NewFingerprintTimeRangeIndex(basePath string) (*FingerprintTimeRangeIndex, error) {
	fingerprintTimeRangeDB, err := NewLevelDB(LevelDBOptions{
		Path:           path.Join(basePath, fingerprintTimeRangeDir),
		CacheSizeBytes: *fingerprintTimeRangeCacheSize,
	})
	if err != nil {
		return nil, err
	}
	return &FingerprintTimeRangeIndex{
		KeyValueStore: fingerprintTimeRangeDB,
	}, nil
}
