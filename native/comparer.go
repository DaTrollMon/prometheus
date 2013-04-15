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

package native

// #cgo LDFLAGS: -lleveldb -lprotobuf-c
// #include <assert.h>
// #include <stdlib.h>
// #include "leveldb/c.h"
// #include "data.pb-c.h"
//
// static void CmpDestroy(void *unused) {
// }
//
// static int CmpCompare(void *unused, const char *left, size_t llen,
//                       const char *right, size_t rlen) {
//
//   assert(left != NULL);
//   assert(right != NULL);
//
//   Dto__SampleKey *left_key;
//   Dto__SampleKey *right_key;
//
//   left_key = dto__sample_key__unpack(NULL, llen, (const uint8_t *)left);
//   right_key = dto__sample_key__unpack(NULL, llen, (const uint8_t *)right);
//
//   assert(left_key != NULL);
//   assert(right_key != NULL);
//
//   int value = 0;
//
//   if (left_key->fingerprint->hash < right_key->fingerprint->hash) {
//     value = -1;
//   } else if (left_key->fingerprint->hash > right_key->fingerprint->hash) {
//     value = 1;
//   } else if (*left_key->fingerprint->first < *right_key->fingerprint->first) {
//     value = -1;
//   } else if (*left_key->fingerprint->first > *right_key->fingerprint->first) {
//     value = 1;
//   } else if (left_key->fingerprint->modulus < right_key->fingerprint->modulus) {
//     value = -1;
//   } else if (left_key->fingerprint->modulus > right_key->fingerprint->modulus) {
//     value = 1;
//   } else if (*left_key->fingerprint->last < *right_key->fingerprint->last) {
//     value = -1;
//   } else if (*left_key->fingerprint->last > *right_key->fingerprint->last) {
//     value = 1;
//   } else if (left_key->timestamp < right_key->timestamp) {
//     value = -1;
//   } else if (left_key->timestamp > right_key->timestamp) {
//     value = 1;
//   }
//
//   // N.B.(matt): These explicitly reap the nested fields recursively.
//   dto__sample_key__free_unpacked(left_key, NULL);
//   dto__sample_key__free_unpacked(right_key, NULL);
//
//   return value;
// }
//
// static const char * CmpName(void *unused) {
//   return "SampleKeyComparator";
// }
//
// static leveldb_comparator_t *CmpNew() {
//   return leveldb_comparator_create(NULL, CmpDestroy, CmpCompare, CmpName);
// }
import "C"

// N.B.(matt): This import block must exist self-standing away from the others
//             due to cgo AST inspection black magic.
import (
	"unsafe"
)

// SampleKeyComparator provides a LevelDB comparator implemented in C and bound
// to Go via the cgo bindings which sorts lexicographically/numerically based on
// the following fields in order of priority:
//
// Fingerprint Hash: The FNV-1A 64 bit integer for the fingerprints.
// Fingerprint First Label Name Letter: The first letter of the first label
//                                      name.
// Fingerprint Label Matter Modulus: A single digit of the modulus of the
//                                   metric's label name and value parts.
// Fingerprint Last Label Name Letter: The last letter of the last label value.
// Timestamp: The supertime for the sample group.
type SampleKeyComparator struct {
	Comparator *C.leveldb_comparator_t
}

func NewSampleKeyComparator() SampleKeyComparator {
	return SampleKeyComparator{
		Comparator: C.CmpNew(),
	}
}

func (c SampleKeyComparator) Close() {
	C.leveldb_comparator_destroy(c.Comparator)
}

func compare(left string, llen int, right string, rlen int) int {
	leftPtr := C.CString(left)
	rightPtr := C.CString(right)

	defer C.free(unsafe.Pointer(leftPtr))
	defer C.free(unsafe.Pointer(rightPtr))

	return int(C.int(C.CmpCompare(nil, leftPtr, C.size_t(llen), rightPtr, C.size_t(rlen))))
}
